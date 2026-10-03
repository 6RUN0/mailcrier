// Package discord implements the discord target type: one execution of a
// channel webhook per message, with the attachments as files.
package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"

	"github.com/6RUN0/mailcrier/internal/backend"
	"github.com/6RUN0/mailcrier/internal/text"
)

// Limits of Discord (developers/resources/webhook.mdx, message.mdx and
// reference.mdx of discord-api-docs): content up to 2000 characters, up to
// 10 files, 20 MiB per file by default, 25 MiB per request. The content is
// counted in UTF-16 units, at least the number of characters, so a
// character outside the BMP cannot push it over. maxFilesSize keeps
// payloadReserve of the request for the JSON part and the multipart
// headers.
const (
	maxText        = 2000
	maxFiles       = 10
	maxFileSize    = 20 << 20
	payloadReserve = 64 << 10
	maxFilesSize   = 25<<20 - payloadReserve
)

// Options configure one discord target.
type Options struct {
	// URL is the webhook URL; its path holds the webhook token.
	URL string
	// Client performs the requests; New uses a copy that does not follow
	// redirects.
	Client *http.Client
}

// Sender executes one Discord webhook.
type Sender struct {
	opts Options
}

// New returns a Sender for opts.
func New(opts Options) *Sender {
	opts.Client = backend.WithoutRedirects(opts.Client)
	return &Sender{opts: opts}
}

// Caps reports the limits of a Discord webhook.
func (s *Sender) Caps() backend.Caps {
	return backend.Caps{MaxText: maxText, Measure: text.UTF16Len, MaxFiles: maxFiles, MaxFileSize: maxFileSize, MaxFilesSize: maxFilesSize}
}

// Send executes the webhook with the text and the files in one request.
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
	if backend.IsSuccess(resp.StatusCode) {
		return nil
	}
	return backend.StatusError(resp)
}

// execution is the JSON part of a webhook execution.
type execution struct {
	Content         string          `json:"content"`
	AllowedMentions allowedMentions `json:"allowed_mentions"`
	Attachments     []attachment    `json:"attachments,omitempty"`
}

// allowedMentions with an empty, non-nil Parse lets no mention ping:
// neither @everyone and @here nor a user or role written in the message.
type allowedMentions struct {
	Parse []string `json:"parse"`
}

type attachment struct {
	ID       int    `json:"id"`
	Filename string `json:"filename"`
}

// buildRequest returns the webhook execution for p. wait=true makes
// Discord answer only after the message is stored, with an error status
// when it is not; without it a 204 comes before validation. A query of
// the configured URL, such as thread_id, is kept. Without files the body
// is JSON, with files multipart with the JSON in payload_json and the
// files as files[n].
func buildRequest(ctx context.Context, opts Options, p backend.Payload) (*http.Request, error) {
	endpoint, err := url.Parse(opts.URL)
	if err != nil {
		return nil, fmt.Errorf("parse webhook URL: %w", backend.WithoutURL(err))
	}
	query := endpoint.Query()
	query.Set("wait", "true")
	endpoint.RawQuery = query.Encode()
	msg := execution{Content: p.Text, AllowedMentions: allowedMentions{Parse: []string{}}}
	for i, file := range p.Attachments {
		msg.Attachments = append(msg.Attachments, attachment{ID: i, Filename: file.Name})
	}
	payload, err := json.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("encode message: %w", err)
	}
	body, contentType := payload, "application/json"
	if len(p.Attachments) > 0 {
		body, contentType, err = multipartBody(payload, p.Attachments)
		if err != nil {
			return nil, fmt.Errorf("encode files: %w", err)
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", backend.WithoutURL(err))
	}
	req.Header.Set("Content-Type", contentType)
	return req, nil
}

// multipartBody returns the multipart form of a webhook execution with
// files and its content type.
func multipartBody(payload []byte, files []backend.Attachment) ([]byte, string, error) {
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition", `form-data; name="payload_json"`)
	header.Set("Content-Type", "application/json")
	part, err := form.CreatePart(header)
	if err != nil {
		return nil, "", err
	}
	if _, err := part.Write(payload); err != nil {
		return nil, "", err
	}
	for i, file := range files {
		if err := backend.WriteFilePart(form, fmt.Sprintf("files[%d]", i), file); err != nil {
			return nil, "", err
		}
	}
	if err := form.Close(); err != nil {
		return nil, "", err
	}
	return body.Bytes(), form.FormDataContentType(), nil
}
