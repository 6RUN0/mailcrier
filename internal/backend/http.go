package backend

import (
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
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

// WithoutURL drops the request URL that net/http puts into its errors.
func WithoutURL(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return fmt.Errorf("%s: %w", urlErr.Op, urlErr.Err)
	}
	return err
}

// StatusError returns the *Error of a response outside 2xx, classified by
// its status and Retry-After header. The body is not quoted: it may echo
// the request, and the request carries the message.
func StatusError(resp *http.Response) *Error {
	return &Error{
		Class:      Classify(resp.StatusCode, resp.Header, nil),
		Status:     resp.StatusCode,
		RetryAfter: RetryAfter(resp.Header),
		Err:        errors.New(http.StatusText(resp.StatusCode)),
	}
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
