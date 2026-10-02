// Package webhook implements the http target type: one request per
// message, POST unless configured otherwise, with the JSON document its
// template renders, or without a body for GET. The path, query and
// header values of the request come rendered in Payload.Request; scheme,
// host and port only from the configured URL.
package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"golang.org/x/net/http/httpguts"

	"github.com/6RUN0/slendmail/internal/backend"
	"github.com/6RUN0/slendmail/internal/text"
)

// Options configure one http target.
type Options struct {
	// URL is the endpoint; it may embed a secret.
	URL string
	// Method is the request method; empty means POST. A GET carries no
	// body and no Content-Type.
	Method string
	// Format is the markup of the preset template; it selects the length
	// limit.
	Format text.Format
	// Fields are string members added to the top-level object of the
	// document, replacing members of the same name: the username and
	// channel of a Mattermost webhook.
	Fields map[string]string
	// Client performs the requests; it must not be nil. New uses a copy
	// that does not follow redirects.
	Client *http.Client
}

// Sender sends the rendered payload to the configured URL.
type Sender struct {
	opts Options
}

// New returns a Sender for opts; it does not follow redirects, see
// backend.WithoutRedirects.
func New(opts Options) *Sender {
	opts.Client = backend.WithoutRedirects(opts.Client)
	return &Sender{opts: opts}
}

// slackWebhookMaxText is the limit of the slack-webhook preset: Slack
// truncates a message over 40000 characters (docs.slack.dev,
// chat.postMessage, "Truncating content"; the incoming webhook page names
// no limit of its own). It applies to the whole JSON document, counted in
// UTF-16 units, which the text inside never exceeds.
const slackWebhookMaxText = 40000

// Caps reports no files, since a JSON document carries none, and a length
// limit only for slack-webhook. Mattermost splits a long text of an
// incoming webhook into several posts, and the receiver of generic-json
// is unknown, so both have none.
func (s *Sender) Caps() backend.Caps {
	if s.opts.Format == text.FormatSlackWebhook && s.opts.Method != http.MethodGet {
		return backend.Caps{MaxText: slackWebhookMaxText, Measure: text.UTF16Len}
	}
	return backend.Caps{}
}

// Send sends p and classifies the outcome.
func (s *Sender) Send(ctx context.Context, p backend.Payload) error {
	req, err := buildRequest(ctx, s.opts, p)
	if err != nil {
		return &backend.Error{Class: backend.Permanent, Err: err}
	}
	resp, err := s.opts.Client.Do(req)
	if err != nil {
		return backend.TransportError(err)
	}
	defer backend.Drain(resp.Body)
	return parseResponse(resp)
}

// buildRequest sends p.Text, the JSON document of the template, with
// opts.Fields added, or no body for a GET, to the configured URL with the
// rendered parts of p.Request. A header that config.Load would reject, or
// that a template rendered with a line break or NUL, fails here: net/http
// refuses it only in Client.Do, as a transport error, which would be
// retried in vain.
func buildRequest(ctx context.Context, opts Options, p backend.Payload) (*http.Request, error) {
	parts := p.Request
	if parts == nil {
		parts = &backend.Request{}
	}
	for name, value := range parts.Headers {
		if !httpguts.ValidHeaderFieldName(name) || !httpguts.ValidHeaderFieldValue(value) {
			return nil, fmt.Errorf("header %q is invalid", name)
		}
	}
	target, err := buildURL(opts.URL, parts.Path, parts.Query)
	if err != nil {
		return nil, err
	}
	method := opts.Method
	if method == "" {
		method = http.MethodPost
	}
	var body io.Reader
	if method != http.MethodGet {
		document := p.Text
		if len(opts.Fields) > 0 {
			if document, err = withFields(document, opts.Fields); err != nil {
				return nil, err
			}
		}
		body = strings.NewReader(document)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", backend.WithoutURL(err))
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for name, value := range parts.Headers {
		req.Header.Set(name, value)
	}
	return req, nil
}

// buildURL returns raw with path appended to its path and query to its
// query. Scheme, host, port and user information stay those of raw, which
// the result is checked against. The errors quote neither the URL, which
// may hold a token, nor the path, which comes from the message.
func buildURL(raw, path string, query url.Values) (string, error) {
	if path == "" && len(query) == 0 {
		return raw, nil
	}
	base, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("the configured URL does not parse")
	}
	built := *base
	if path != "" {
		if err := checkPath(path); err != nil {
			return "", err
		}
		built.RawPath = strings.TrimSuffix(base.EscapedPath(), "/") + escapePath(path)
		if built.Path, err = url.PathUnescape(built.RawPath); err != nil {
			return "", errors.New("path: invalid escape")
		}
	}
	if len(query) > 0 {
		built.RawQuery = strings.TrimPrefix(base.RawQuery+"&"+query.Encode(), "&")
	}
	result := built.String()
	check, err := url.Parse(result)
	if err != nil || check.Scheme != base.Scheme || check.Host != base.Host || check.User.String() != base.User.String() {
		return "", errors.New("path or query changes the scheme, host, port or user of the URL")
	}
	return result, nil
}

// checkPath rejects a rendered path that does not start with exactly one
// slash, as "//host/" would name another host, that carries a query or
// fragment of its own, or that, once unescaped, holds a dot segment, which
// would climb out of the path of the configured URL, a control character
// or a backslash, which some servers take for a slash. A dot segment
// counts with a parameter after ";", as servlet containers strip it, and
// when it shows only after a second unescape of the part before ";", as a
// proxy that unescapes once more would see it, whatever the parameter
// holds.
func checkPath(path string) error {
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") {
		return errors.New("path: must start with a single /")
	}
	if strings.ContainsAny(path, "?#") {
		return errors.New("path: must not hold ? or #")
	}
	decoded, err := url.PathUnescape(path)
	if err != nil {
		return errors.New("path: invalid escape")
	}
	if strings.ContainsFunc(decoded, func(r rune) bool { return r < 0x20 || r == 0x7f || r == '\\' }) {
		return errors.New("path: must not hold a control character or a backslash")
	}
	for _, segment := range strings.Split(decoded, "/") {
		if isDotSegment(segment) {
			return errors.New("path: must not hold a . or .. segment")
		}
		name, _, _ := strings.Cut(segment, ";")
		if twice, err := url.PathUnescape(name); err == nil && twice != name {
			for _, inner := range strings.Split(twice, "/") {
				if isDotSegment(inner) {
					return errors.New("path: must not hold a . or .. segment")
				}
			}
		}
	}
	return nil
}

// isDotSegment reports whether segment, without a parameter after ";", is
// "." or "..".
func isDotSegment(segment string) bool {
	name, _, _ := strings.Cut(segment, ";")
	return name == "." || name == ".."
}

// escapePath returns a path that checkPath accepted with every segment,
// split at the raw slashes, unescaped and escaped again as a path
// segment, so that the result is always a valid escaped path: an escaped
// slash then stays one, where net/url would escape the unescaped path
// anew and turn it into a separator. The characters a segment may hold
// as they are (RFC 3986 pchar) stay as written.
func escapePath(path string) string {
	segments := strings.Split(path, "/")
	for i, segment := range segments {
		decoded, err := url.PathUnescape(segment)
		if err != nil {
			continue
		}
		var b strings.Builder
		for _, c := range []byte(decoded) {
			if isPathChar(c) {
				b.WriteByte(c)
			} else {
				fmt.Fprintf(&b, "%%%02X", c)
			}
		}
		segments[i] = b.String()
	}
	return strings.Join(segments, "/")
}

// isPathChar reports whether c may stand unescaped in a path segment:
// unreserved, a sub-delimiter, ":" or "@" (RFC 3986, section 3.3).
func isPathChar(c byte) bool {
	switch {
	case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		return true
	}
	return strings.IndexByte("-._~!$&'()*+,;=:@", c) >= 0
}

// withFields returns the JSON object document with fields added as string
// members. The members keep their encoding, without escaping <, > and &
// again; the order becomes sorted by name.
func withFields(document string, fields map[string]string) (string, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal([]byte(document), &object); err != nil {
		return "", fmt.Errorf("template output is not a JSON object: %w", err)
	}
	if object == nil {
		return "", errors.New("template output is not a JSON object: null")
	}
	for name, value := range fields {
		encoded, err := json.Marshal(value)
		if err != nil {
			return "", err
		}
		object[name] = encoded
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(object); err != nil {
		return "", err
	}
	return strings.TrimSuffix(b.String(), "\n"), nil
}

// parseResponse accepts any 2xx status.
func parseResponse(resp *http.Response) error {
	if backend.IsSuccess(resp.StatusCode) {
		return nil
	}
	return backend.StatusError(resp)
}
