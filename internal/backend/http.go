package backend

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"time"
)

// WithoutRedirects returns a copy of client that does not follow
// redirects. net/http turns a redirected POST into a GET without body, so
// the receiver would answer 200 to a request that never carried the
// message; the caller sees the 3xx instead, a permanent failure.
func WithoutRedirects(client *http.Client) *http.Client {
	c := *client
	c.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &c
}

// TransportError returns the error of an HTTP client call, which never
// carries a response, as a temporary *Error without the request URL: the
// URL of a webhook or of the Telegram Bot API holds the token.
func TransportError(err error) *Error {
	return &Error{Class: Classify(0, nil, err), Err: WithoutURL(err)}
}

// RequestError is TransportError for the call of client that sent req,
// with the configured bound that ended it in place of the cause, such as
// "Post: deadline 30s exceeded": the operator learns what to raise.
func RequestError(client *http.Client, req *http.Request, err error) *Error {
	e := TransportError(err)
	if limit := limitOf(req.Context(), client.Timeout, err); limit != nil {
		op := "request"
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			op = urlErr.Op
		}
		e.Err = fmt.Errorf("%s: %w", op, limit)
	}
	return e
}

// limitOf returns the bound that ended a request with err, nil when none
// did: the cause of ctx, such as a *LimitError or a signal, when ctx ended
// with one, else http_timeout when the timeout of the client did. net/http
// tells its timeout only by the text "Client.Timeout", while reading the
// body as "Client.Timeout or context cancellation": every context of a
// delivery ends with a cause, so the latter is the timeout too. A dial
// timeout of the transport has another text.
func limitOf(ctx context.Context, timeout time.Duration, err error) error {
	if ctx.Err() != nil {
		if cause := context.Cause(ctx); cause != ctx.Err() {
			return cause
		}
	}
	if timeout > 0 && strings.Contains(err.Error(), "Client.Timeout") {
		return &LimitError{Key: "http_timeout", Value: timeout}
	}
	return nil
}

// clientTimeoutKey is the context key of the timeout of the client, which
// Do sets on the request for BodyError.
type clientTimeoutKey struct{}

// BodyError returns the error of reading the body of resp as a temporary
// *Error when the reading failed, not the content: the context of the
// request ended, the connection failed, or the body ended early, which
// net/http reports as io.ErrUnexpectedEOF when the connection closed
// before Content-Length; the cause names the bound as in RequestError. It
// returns nil for any other error, such as JSON that does not parse, which
// the caller describes without quoting the body. A JSON decoder gives
// io.ErrUnexpectedEOF for a whole body cut in the middle of a value too:
// whether the message arrived is unknown then as well, and a duplicate
// costs less than a loss.
func BodyError(resp *http.Response, err error) *Error {
	var netErr net.Error
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) && !errors.As(err, &netErr) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil
	}
	cause := WithoutURL(err)
	if resp.Request != nil {
		ctx := resp.Request.Context()
		timeout, _ := ctx.Value(clientTimeoutKey{}).(time.Duration)
		if limit := limitOf(ctx, timeout, err); limit != nil {
			cause = limit
		}
	}
	return &Error{Class: Temporary, Status: resp.StatusCode, Err: fmt.Errorf("answer not read: %w", cause)}
}

// WithoutURL drops the request URL that net/http puts into its errors.
func WithoutURL(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return fmt.Errorf("%s: %w", urlErr.Op, urlErr.Err)
	}
	return err
}

// StatusError returns the *Error of a response outside 2xx, classified by
// its status and Retry-After header, with StatusCause as the cause.
func StatusError(resp *http.Response) *Error {
	return &Error{
		Class:      Classify(resp.StatusCode, resp.Header, nil),
		Status:     resp.StatusCode,
		RetryAfter: RetryAfter(resp.Header),
		Err:        StatusCause(resp.StatusCode, resp.Header),
	}
}

// StatusCause describes a response outside 2xx by its status text. The
// body is not quoted: it may echo the request, and the request carries
// the message. A redirect names the scheme and host of its Location,
// "redirect to https://ntfy.example.org not followed", which tells a
// moved service; the path and query are left out, since they repeat the
// path of the target, where a topic or a key of any length is a secret
// the redactor does not know.
func StatusCause(status int, header http.Header) error {
	if status < 300 || status >= 400 {
		return errors.New(http.StatusText(status))
	}
	location, err := url.Parse(header.Get("Location"))
	switch {
	case err != nil || header.Get("Location") == "":
		return errors.New("redirect not followed")
	case location.Host == "":
		return errors.New("redirect to another path not followed")
	}
	host := location.Host
	if len(host) > maxLocationHost {
		host = strings.ToValidUTF8(host[:maxLocationHost], "") + "..."
	}
	return fmt.Errorf("redirect to %s://%s not followed", location.Scheme, host)
}

// maxLocationHost bounds the host of a Location quoted in the error of a
// redirect, a header of the service; a DNS name has at most 253 bytes.
const maxLocationHost = 253

// Do sends req with client and returns the outcome as an *Error: a
// transport failure by TransportError, the response by parse, or, with a
// nil parse, nil for 2xx and StatusError otherwise. The body is drained
// after parse returns, so parse may read it.
func Do(client *http.Client, req *http.Request, parse func(*http.Response) error) error {
	req = req.WithContext(context.WithValue(req.Context(), clientTimeoutKey{}, client.Timeout))
	resp, err := client.Do(req)
	if err != nil {
		return RequestError(client, req, err)
	}
	defer Drain(resp.Body)
	if parse != nil {
		return parse(resp)
	}
	if IsSuccess(resp.StatusCode) {
		return nil
	}
	return StatusError(resp)
}

// MaxDrainBytes bounds how much of a response body is read and thrown
// away, so that an endless response cannot hold the process.
const MaxDrainBytes = 4 << 10

// Drain reads and discards at most MaxDrainBytes of body and closes it,
// which lets the transport reuse the connection for a short answer.
func Drain(body io.ReadCloser) {
	_, _ = io.CopyN(io.Discard, body, MaxDrainBytes)
	_ = body.Close()
}

// IsSuccess reports a 2xx status.
func IsSuccess(status int) bool {
	return status >= 200 && status < 300
}

// fileNameEscaper escapes a file name for a quoted Content-Disposition
// parameter, as multipart.Writer.CreateFormFile does.
var fileNameEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`)

// WriteFilePart adds file to form as the part named field, with its
// content type, application/octet-stream when unknown; CreateFormFile
// would always set the latter.
func WriteFilePart(form *multipart.Writer, field string, file Attachment) error {
	contentType := file.ContentType
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`, fileNameEscaper.Replace(field), fileNameEscaper.Replace(file.Name)))
	header.Set("Content-Type", contentType)
	part, err := form.CreatePart(header)
	if err != nil {
		return err
	}
	_, err = part.Write(file.Data)
	return err
}
