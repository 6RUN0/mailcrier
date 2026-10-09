// Package slack implements the slack target type through the Slack Web
// API: chat.postMessage for the text and the external upload methods for
// the attachments, which land in the thread of the message.
//
// The four methods are plain HTTP calls. A client library would add a
// dependency and error texts outside this package's control; the upload
// URL that Slack hands out is signed and must not reach the log.
package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/6RUN0/mailcrier/internal/backend"
	"github.com/6RUN0/mailcrier/internal/text"
)

// DefaultAPIURL is the Web API endpoint.
const DefaultAPIURL = "https://slack.com/api"

// Limits: chat.postMessage truncates a text over 40000 characters
// (docs.slack.dev, "Truncating content"); counted in UTF-16 units, at
// least the number of characters. maxFiles bounds the three requests each
// file costs. Slack publishes no file size limit below the size of a
// message mailcrier reads.
const (
	maxText  = 40000
	maxFiles = 10
)

// maxResponseBytes bounds the answer read: chat.postMessage echoes the
// message.
const maxResponseBytes = 1 << 20

// maxErrorCodeBytes bounds the error code quoted from an answer.
const maxErrorCodeBytes = 128

// temporaryErrors are the error codes of a Web API answer with status 200
// that a later attempt may not get.
var temporaryErrors = map[string]bool{
	"ratelimited": true, "internal_error": true, "fatal_error": true, "service_unavailable": true, "request_timeout": true,
}

// Options configure one slack target.
type Options struct {
	// Token is the bot token, sent as a bearer token.
	Token string
	// Channel is the channel ID or name the bot posts to.
	Channel string
	// APIURL is the Web API endpoint without a trailing slash;
	// DefaultAPIURL when empty.
	APIURL string
	// Client performs the requests; New uses a copy that does not follow
	// redirects.
	Client *http.Client
}

// Sender posts to one Slack channel.
type Sender struct {
	opts Options
}

// New returns a Sender for opts.
func New(opts Options) *Sender {
	if opts.APIURL == "" {
		opts.APIURL = DefaultAPIURL
	}
	opts.Client = backend.WithoutRedirects(opts.Client)
	return &Sender{opts: opts}
}

// Caps reports the limits of chat.postMessage and the file count.
func (s *Sender) Caps() backend.Caps {
	return backend.Caps{MaxText: maxText, Measure: text.UTF16Len, MaxFiles: maxFiles}
}

// answer holds the fields of the Web API answers this package reads.
type answer struct {
	OK        bool   `json:"ok"`
	Error     string `json:"error"`
	Channel   string `json:"channel"`
	TS        string `json:"ts"`
	UploadURL string `json:"upload_url"`
	FileID    string `json:"file_id"`
}

// Send posts the text, then uploads the attachments into its thread.
// When the text arrived and a file did not, the error is partial.
func (s *Sender) Send(ctx context.Context, p backend.Payload) error {
	req, err := buildRequest(ctx, s.opts, p)
	if err != nil {
		return &backend.Error{Class: backend.Permanent, Err: err}
	}
	var posted answer
	if err := s.call(req, &posted); err != nil {
		return err
	}
	if len(p.Attachments) == 0 {
		return nil
	}
	if err := s.upload(ctx, p.Attachments, posted); err != nil {
		var deliveryErr *backend.Error
		if !errors.As(err, &deliveryErr) {
			deliveryErr = &backend.Error{Class: backend.Permanent, Err: err}
		}
		deliveryErr.IsPartial = true
		return deliveryErr
	}
	return nil
}

// upload sends files with files.getUploadURLExternal, a POST of the
// content to the returned URL each, and one files.completeUploadExternal
// that shares them in the thread of the posted message.
func (s *Sender) upload(ctx context.Context, files []backend.Attachment, posted answer) error {
	type completed struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	}
	var done []completed
	for i, file := range files {
		form := url.Values{"filename": {file.Name}, "length": {strconv.Itoa(len(file.Data))}}
		req, err := s.formRequest(ctx, "files.getUploadURLExternal", form)
		if err != nil {
			return inFile(i, len(files), err)
		}
		var target answer
		if err := s.call(req, &target); err != nil {
			return inFile(i, len(files), err)
		}
		if err := s.post(ctx, target.UploadURL, file); err != nil {
			return inFile(i, len(files), err)
		}
		done = append(done, completed{ID: target.FileID, Title: file.Name})
	}
	list, err := json.Marshal(done)
	if err != nil {
		return err
	}
	req, err := s.formRequest(ctx, "files.completeUploadExternal", url.Values{
		"files": {string(list)}, "channel_id": {posted.Channel}, "thread_ts": {posted.TS},
	})
	if err != nil {
		return err
	}
	return s.call(req, &answer{})
}

// inFile names file i of n in the cause of err, a *backend.Error.
func inFile(i, n int, err error) error {
	var deliveryErr *backend.Error
	if errors.As(err, &deliveryErr) {
		deliveryErr.Err = fmt.Errorf("file %d of %d: %w", i+1, n, deliveryErr.Err)
	}
	return err
}

// post sends the content of file to the signed upload URL. The URL is
// never quoted in an error.
func (s *Sender) post(ctx context.Context, uploadURL string, file backend.Attachment) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadURL, bytes.NewReader(file.Data))
	if err != nil {
		return &backend.Error{Class: backend.Permanent, Err: fmt.Errorf("build upload: %w", backend.WithoutURL(err))}
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	return backend.Do(s.opts.Client, req, nil)
}

// postMessage is the body of chat.postMessage. Unfurling is off: a URL in a
// cron report would otherwise pull a preview of the site into the channel.
type postMessage struct {
	Channel     string `json:"channel"`
	Text        string `json:"text"`
	UnfurlLinks bool   `json:"unfurl_links"`
	UnfurlMedia bool   `json:"unfurl_media"`
}

// buildRequest returns the chat.postMessage request for p.Text, which is
// in mrkdwn with &, < and > escaped, so that <!channel> in a message stays
// text.
func buildRequest(ctx context.Context, opts Options, p backend.Payload) (*http.Request, error) {
	body, err := json.Marshal(postMessage{Channel: opts.Channel, Text: p.Text})
	if err != nil {
		return nil, fmt.Errorf("encode chat.postMessage: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, opts.APIURL+"/chat.postMessage", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", backend.WithoutURL(err))
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", "Bearer "+opts.Token)
	return req, nil
}

// formRequest returns a form-encoded request of a Web API method.
func (s *Sender) formRequest(ctx context.Context, method string, form url.Values) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.opts.APIURL+"/"+method, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, &backend.Error{Class: backend.Permanent, Err: fmt.Errorf("build request: %w", backend.WithoutURL(err))}
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Bearer "+s.opts.Token)
	return req, nil
}

// call performs one Web API request and decodes the answer into into.
func (s *Sender) call(req *http.Request, into *answer) error {
	return backend.Do(s.opts.Client, req, func(resp *http.Response) error {
		return parseResponse(resp, into)
	})
}

// parseResponse decodes a Web API answer. A status outside 2xx fails by
// its status and Retry-After; with 2xx the "ok" field decides, and the
// error code of a failure is temporary when it is in temporaryErrors. An
// answer that is not JSON with 2xx is temporary: whether the message
// arrived is unknown, and a duplicate costs less than a loss.
func parseResponse(resp *http.Response, into *answer) error {
	if !backend.IsSuccess(resp.StatusCode) {
		return backend.StatusError(resp)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(into); err != nil {
		return &backend.Error{Class: backend.Temporary, Status: resp.StatusCode, Err: errors.New("answer is not JSON")}
	}
	if into.OK {
		return nil
	}
	code := strings.ToValidUTF8(text.TruncateBytes(into.Error, maxErrorCodeBytes), "")
	class := backend.Permanent
	if temporaryErrors[code] {
		class = backend.Temporary
	}
	if code == "" {
		code = "no error code"
	}
	return &backend.Error{Class: class, RetryAfter: backend.RetryAfter(resp.Header), Err: errors.New(code)}
}
