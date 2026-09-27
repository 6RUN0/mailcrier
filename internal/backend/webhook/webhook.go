// Package webhook implements the http target type: one POST of a JSON
// document per message.
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

	"github.com/6RUN0/slendmail/internal/backend"
)

// Options configure one http target.
type Options struct {
	// URL is the endpoint; it may embed a secret.
	URL string
	// Hostname is sent along with the message so that the receiver can
	// tell which machine produced it.
	Hostname string
	// Client performs the requests; it must not be nil. New uses a copy
	// that does not follow redirects.
	Client *http.Client
}

// Sender posts the generic-json payload to the configured URL.
type Sender struct {
	opts Options
}

// New returns a Sender for opts.
//
// Redirects are not followed: net/http turns a redirected POST into a GET
// without body, so the receiver would answer 200 to a request that never
// carried the message. A 3xx answer is a permanent failure instead.
func New(opts Options) *Sender {
	client := *opts.Client
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	opts.Client = &client
	return &Sender{opts: opts}
}

// Caps reports no limits: the receiver of a generic webhook is unknown.
func (s *Sender) Caps() backend.Caps {
	return backend.Caps{}
}

// genericJSON is the body of the generic-json preset.
type genericJSON struct {
	Subject  string `json:"subject"`
	Body     string `json:"body"`
	Hostname string `json:"hostname"`
}

// Send posts p and classifies the outcome.
func (s *Sender) Send(ctx context.Context, p backend.Payload) error {
	req, err := buildRequest(ctx, s.opts, p)
	if err != nil {
		return &backend.Error{Class: backend.Permanent, Err: err}
	}
	resp, err := s.opts.Client.Do(req)
	if err != nil {
		return &backend.Error{Class: backend.Classify(0, nil, err), Err: withoutURL(err)}
	}
	defer func() { _ = resp.Body.Close() }()
	return parseResponse(resp)
}

func buildRequest(ctx context.Context, opts Options, p backend.Payload) (*http.Request, error) {
	body, err := json.Marshal(genericJSON{Subject: p.Title, Body: p.Text, Hostname: opts.Hostname})
	if err != nil {
		return nil, fmt.Errorf("encode payload: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, opts.URL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", withoutURL(err))
	}
	req.Header.Set("Content-Type", "application/json")
	return req, nil
}

// maxDrainBytes bounds how much of a response body is read and thrown away,
// so that an endless response cannot hold the process.
const maxDrainBytes = 4 << 10

// parseResponse accepts any 2xx status. The response body is not reported:
// it may echo the request, and the request carries the message.
func parseResponse(resp *http.Response) error {
	_, _ = io.CopyN(io.Discard, resp.Body, maxDrainBytes)
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	return &backend.Error{
		Class:      backend.Classify(resp.StatusCode, resp.Header, nil),
		RetryAfter: backend.RetryAfter(resp.Header),
		Status:     resp.StatusCode,
		Err:        errors.New(http.StatusText(resp.StatusCode)),
	}
}

// withoutURL drops the request URL that net/http puts into its errors:
// webhook URLs carry their token in the path.
func withoutURL(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return fmt.Errorf("%s: %w", urlErr.Op, urlErr.Err)
	}
	return err
}
