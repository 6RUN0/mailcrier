// Package ntfy implements the ntfy target type: a JSON publish for the
// text and one more message per attachment, which ntfy allows one of.
package ntfy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/6RUN0/mailcrier/internal/backend"
	"github.com/6RUN0/mailcrier/internal/text"
)

// Limits of an ntfy server with the default configuration (server/config.go
// and docs/publish.md of binwiederhier/ntfy): a message of 4096 bytes,
// longer ones become an attachment; a title of 1 KB, longer ones get 400;
// attachments of 15 MB. maxFiles bounds the messages one mail produces.
const (
	maxText     = 4096
	maxTitle    = 1024
	maxFiles    = 10
	maxFileSize = 15 << 20
)

// Options configure one ntfy target.
type Options struct {
	// URL is the topic URL, https://ntfy.sh/<topic>; user information and
	// a query, such as auth, are kept for every request.
	URL string
	// Client performs the requests; New uses a copy that does not follow
	// redirects.
	Client *http.Client
}

// Sender publishes to one ntfy topic.
type Sender struct {
	opts Options
	// root is the server URL for the JSON publish, topic the last path
	// segment of the configured URL.
	root  *url.URL
	topic string
}

// New returns a Sender for opts. A URL without a topic is an error: the
// server would take the text for a topic name.
func New(opts Options) (*Sender, error) {
	endpoint, err := url.Parse(opts.URL)
	if err != nil {
		return nil, fmt.Errorf("parse URL: %w", backend.WithoutURL(err))
	}
	path := strings.TrimSuffix(endpoint.Path, "/")
	slash := strings.LastIndexByte(path, '/')
	if slash < 0 || path[slash+1:] == "" {
		return nil, errors.New("URL has no topic")
	}
	root := *endpoint
	root.Path, root.RawPath = path[:slash+1], ""
	opts.Client = backend.WithoutRedirects(opts.Client)
	return &Sender{opts: opts, root: &root, topic: path[slash+1:]}, nil
}

// Caps reports the limits of a default ntfy server.
func (s *Sender) Caps() backend.Caps {
	return backend.Caps{MaxText: maxText, Measure: text.ByteLen, MaxFiles: maxFiles, MaxFileSize: maxFileSize}
}

// Send publishes the text, then each attachment as a message of its own.
// When the text arrived and an attachment did not, the error is partial.
func (s *Sender) Send(ctx context.Context, p backend.Payload) error {
	req, err := s.buildRequest(ctx, p)
	if err != nil {
		return &backend.Error{Class: backend.Permanent, Err: err}
	}
	if err := backend.Do(s.opts.Client, req, nil); err != nil {
		return err
	}
	for i, file := range p.Attachments {
		req, err := s.buildAttachmentRequest(ctx, p.Title, file)
		if err == nil {
			err = backend.Do(s.opts.Client, req, nil)
		} else {
			err = &backend.Error{Class: backend.Permanent, Err: err}
		}
		if err != nil {
			var deliveryErr *backend.Error
			errors.As(err, &deliveryErr)
			deliveryErr.IsPartial = true
			deliveryErr.Err = fmt.Errorf("attachment %d of %d: %w", i+1, len(p.Attachments), deliveryErr.Err)
			return deliveryErr
		}
	}
	return nil
}

// publish is the body of a JSON publish.
type publish struct {
	Topic   string `json:"topic"`
	Title   string `json:"title,omitempty"`
	Message string `json:"message"`
}

// buildRequest returns the JSON publish of p to the server root: the JSON
// form carries a title of any characters, which an HTTP header does not.
func (s *Sender) buildRequest(ctx context.Context, p backend.Payload) (*http.Request, error) {
	body, err := json.Marshal(publish{Topic: s.topic, Title: title(p.Title), Message: p.Text})
	if err != nil {
		return nil, fmt.Errorf("encode publish: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.root.String(), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", backend.WithoutURL(err))
	}
	req.Header.Set("Content-Type", "application/json")
	return req, nil
}

// buildAttachmentRequest returns the PUT of file to the topic. File name
// and title go in the query, where ntfy reads its parameters as well as in
// headers, so that non-ASCII text needs no header encoding.
func (s *Sender) buildAttachmentRequest(ctx context.Context, subject string, file backend.Attachment) (*http.Request, error) {
	endpoint := *s.root
	endpoint.Path += s.topic
	query := endpoint.Query()
	query.Set("filename", file.Name)
	if t := title(subject); t != "" {
		query.Set("title", t)
	}
	endpoint.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint.String(), bytes.NewReader(file.Data))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", backend.WithoutURL(err))
	}
	return req, nil
}

// title returns subject as an ntfy title: line breaks of a folded header
// become spaces, and the text is cut to maxTitle bytes at a character
// boundary, since a longer title gets 400.
func title(subject string) string {
	subject = strings.NewReplacer("\r\n", " ", "\r", " ", "\n", " ").Replace(subject)
	return text.TruncateBytes(subject, maxTitle)
}
