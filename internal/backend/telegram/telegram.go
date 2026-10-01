// Package telegram implements the telegram target type: the Bot API
// methods sendMessage for the text and sendDocument for each attachment.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/6RUN0/slendmail/internal/backend"
	"github.com/6RUN0/slendmail/internal/text"
)

// DefaultAPIURL is the Bot API server.
const DefaultAPIURL = "https://api.telegram.org"

// Limits of the Bot API (https://core.telegram.org/bots/api): the text of
// sendMessage is 1-4096 characters after entity parsing, which
// text.MeasureTelegramHTML counts; sendDocument takes files up to 50 MB. maxFiles keeps a message with
// many attachments from running into the limit of 20 messages a minute in
// a group.
const (
	maxText     = 4096
	maxFiles    = 10
	maxFileSize = 50 * 1000 * 1000
)

// maxResponseBytes bounds the answer read: sendMessage echoes the message,
// at most 4096 characters with entities.
const maxResponseBytes = 1 << 20

// maxDescriptionBytes bounds the error description of the Bot API quoted in
// an error, which reaches the log.
const maxDescriptionBytes = 256

// Options configure one telegram target.
type Options struct {
	// Token is the bot token; it is part of every request URL.
	Token string
	// ChatID is the numeric chat id or the @username of a channel.
	ChatID string
	// MessageThreadID is the forum topic; 0 for none.
	MessageThreadID int64
	// DisableNotification sends the messages silently.
	DisableNotification bool
	// APIURL is the Bot API server without a trailing slash;
	// DefaultAPIURL when empty.
	APIURL string
	// Client performs the requests; New uses a copy that does not follow
	// redirects.
	Client *http.Client
}

// Sender sends to one Telegram chat.
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

// Caps reports the limits of the Bot API.
func (s *Sender) Caps() backend.Caps {
	return backend.Caps{MaxText: maxText, Measure: text.MeasureTelegramHTML, MaxFiles: maxFiles, MaxFileSize: maxFileSize}
}

// Send sends the text, then each attachment as a document. When the text
// arrived and a document did not, the error is partial: sending the
// message again would repeat the text.
func (s *Sender) Send(ctx context.Context, p backend.Payload) error {
	req, err := buildRequest(ctx, s.opts, p)
	if err != nil {
		return &backend.Error{Class: backend.Permanent, Err: err}
	}
	if err := s.do(req); err != nil {
		return err
	}
	for i, file := range p.Attachments {
		req, err := buildDocumentRequest(ctx, s.opts, file)
		if err == nil {
			err = s.do(req)
		} else {
			err = &backend.Error{Class: backend.Permanent, Err: err}
		}
		if err != nil {
			var deliveryErr *backend.Error
			errors.As(err, &deliveryErr)
			deliveryErr.IsPartial = true
			deliveryErr.Err = fmt.Errorf("document %d of %d: %w", i+1, len(p.Attachments), deliveryErr.Err)
			return deliveryErr
		}
	}
	return nil
}

// do performs one Bot API request.
func (s *Sender) do(req *http.Request) error {
	resp, err := s.opts.Client.Do(req)
	if err != nil {
		return backend.TransportError(err)
	}
	defer backend.Drain(resp.Body)
	return parseResponse(resp)
}

// sendMessage is the body of the sendMessage method.
type sendMessage struct {
	ChatID              string             `json:"chat_id"`
	MessageThreadID     int64              `json:"message_thread_id,omitempty"`
	Text                string             `json:"text"`
	ParseMode           string             `json:"parse_mode"`
	LinkPreviewOptions  linkPreviewOptions `json:"link_preview_options"`
	DisableNotification bool               `json:"disable_notification,omitempty"`
}

type linkPreviewOptions struct {
	IsDisabled bool `json:"is_disabled"`
}

// buildRequest returns the sendMessage request for p.Text, which is in
// the HTML parse mode. Link previews are off: a URL in a cron report would
// otherwise pull a card from the site into the chat.
func buildRequest(ctx context.Context, opts Options, p backend.Payload) (*http.Request, error) {
	body, err := json.Marshal(sendMessage{
		ChatID:              opts.ChatID,
		MessageThreadID:     opts.MessageThreadID,
		Text:                p.Text,
		ParseMode:           "HTML",
		LinkPreviewOptions:  linkPreviewOptions{IsDisabled: true},
		DisableNotification: opts.DisableNotification,
	})
	if err != nil {
		return nil, fmt.Errorf("encode sendMessage: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, methodURL(opts, "sendMessage"), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", backend.WithoutURL(err))
	}
	req.Header.Set("Content-Type", "application/json")
	return req, nil
}

// buildDocumentRequest returns the sendDocument request for file.
func buildDocumentRequest(ctx context.Context, opts Options, file backend.Attachment) (*http.Request, error) {
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	fields := [][2]string{{"chat_id", opts.ChatID}}
	if opts.MessageThreadID != 0 {
		fields = append(fields, [2]string{"message_thread_id", strconv.FormatInt(opts.MessageThreadID, 10)})
	}
	if opts.DisableNotification {
		fields = append(fields, [2]string{"disable_notification", "true"})
	}
	for _, field := range fields {
		if err := form.WriteField(field[0], field[1]); err != nil {
			return nil, fmt.Errorf("encode sendDocument: %w", err)
		}
	}
	err := backend.WriteFilePart(form, "document", file)
	if err == nil {
		err = form.Close()
	}
	if err != nil {
		return nil, fmt.Errorf("encode sendDocument: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, methodURL(opts, "sendDocument"), &body)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", backend.WithoutURL(err))
	}
	req.Header.Set("Content-Type", form.FormDataContentType())
	return req, nil
}

// methodURL returns the URL of a Bot API method; it holds the token.
func methodURL(opts Options, method string) string {
	return opts.APIURL + "/bot" + opts.Token + "/" + method
}

// response is the envelope of every Bot API answer.
type response struct {
	OK          bool   `json:"ok"`
	ErrorCode   int    `json:"error_code"`
	Description string `json:"description"`
	Parameters  struct {
		RetryAfter int64 `json:"retry_after"`
	} `json:"parameters"`
}

// parseResponse checks the "ok" field, not only the status: an answer
// with "ok": false is a failure whatever its status. The error carries
// error_code as Status, falling back to the HTTP status, and the
// description. error_code 429 and 5xx are temporary, like the HTTP
// statuses, with parameters.retry_after as the delay. An answer with a 2xx
// status that is not JSON, or JSON without "ok" and error_code, from a
// proxy for example, is temporary: whether the message arrived is
// unknown, and a duplicate costs less than a loss.
func parseResponse(resp *http.Response) error {
	var answer response
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&answer); err != nil {
		class := backend.Classify(resp.StatusCode, resp.Header, nil)
		if backend.IsSuccess(resp.StatusCode) {
			class = backend.Temporary
		}
		return &backend.Error{Class: class, Status: resp.StatusCode, RetryAfter: backend.RetryAfter(resp.Header), Err: errors.New("answer is not JSON")}
	}
	if answer.OK {
		return nil
	}
	status := answer.ErrorCode
	if status == 0 {
		status = resp.StatusCode
	}
	retryAfter := backend.RetryAfter(resp.Header)
	if answer.Parameters.RetryAfter > 0 {
		retryAfter = time.Duration(min(answer.Parameters.RetryAfter, int64(backend.MaxRetryAfter/time.Second))) * time.Second
	}
	class := backend.Classify(status, resp.Header, nil)
	if retryAfter > 0 || answer.ErrorCode == 0 && backend.IsSuccess(resp.StatusCode) {
		class = backend.Temporary
	}
	description := strings.ToValidUTF8(text.TruncateBytes(answer.Description, maxDescriptionBytes), "")
	if description == "" {
		description = "no description"
	}
	return &backend.Error{Class: class, Status: status, RetryAfter: retryAfter, Err: errors.New(description)}
}
