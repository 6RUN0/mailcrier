// Package webhook implements the http target type: one POST per message
// of the JSON document its preset template renders.
package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/6RUN0/slendmail/internal/backend"
	"github.com/6RUN0/slendmail/internal/text"
)

// Options configure one http target.
type Options struct {
	// URL is the endpoint; it may embed a secret.
	URL string
	// Format is the markup of the preset template; it selects the length
	// limit.
	Format text.Format
	// Fields are string members added to the top-level object of the
	// document, replacing members of the same name: the username and
	// channel of a Mattermost webhook.
	Fields map[string]string
	// Headers are set after Content-Type, so that they override it.
	Headers map[string]string
	// Client performs the requests; it must not be nil. New uses a copy
	// that does not follow redirects.
	Client *http.Client
}

// Sender posts the rendered payload to the configured URL.
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
	if s.opts.Format == text.FormatSlackWebhook {
		return backend.Caps{MaxText: slackWebhookMaxText, Measure: text.UTF16Len}
	}
	return backend.Caps{}
}

// Send posts p and classifies the outcome.
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

// buildRequest posts p.Text, the JSON document of the preset template,
// with opts.Fields added.
func buildRequest(ctx context.Context, opts Options, p backend.Payload) (*http.Request, error) {
	document := p.Text
	if len(opts.Fields) > 0 {
		var err error
		if document, err = withFields(document, opts.Fields); err != nil {
			return nil, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, opts.URL, strings.NewReader(document))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", backend.WithoutURL(err))
	}
	req.Header.Set("Content-Type", "application/json")
	for name, value := range opts.Headers {
		req.Header.Set(name, value)
	}
	return req, nil
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
