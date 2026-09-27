// Package webhook implements the http target type: one POST per message
// of the JSON document its preset template renders.
package webhook

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/6RUN0/slendmail/internal/backend"
)

// Options configure one http target.
type Options struct {
	// URL is the endpoint; it may embed a secret.
	URL string
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

// Caps reports no length limit and no files: the receiver of a webhook is
// unknown, and a JSON document carries no file.
func (s *Sender) Caps() backend.Caps {
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

// buildRequest posts p.Text, the JSON document of the preset template.
func buildRequest(ctx context.Context, opts Options, p backend.Payload) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, opts.URL, strings.NewReader(p.Text))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", backend.WithoutURL(err))
	}
	req.Header.Set("Content-Type", "application/json")
	return req, nil
}

// parseResponse accepts any 2xx status.
func parseResponse(resp *http.Response) error {
	if backend.IsSuccess(resp.StatusCode) {
		return nil
	}
	return backend.StatusError(resp)
}
