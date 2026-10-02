// Package backend holds the types shared by every delivery target: the
// Sender interface, target capabilities, the rendered payload and the
// classified delivery error. It contains no transport code, so delivery
// logic and target implementations can depend on it without cycles.
package backend

import (
	"context"
	"fmt"
	"time"
)

// Sender delivers one rendered payload to one configured target.
type Sender interface {
	// Caps reports the limits the target imposes on a payload.
	Caps() Caps
	// Send delivers the payload. A non-nil error is always *Error, so the
	// caller can decide between retrying and giving up.
	Send(ctx context.Context, p Payload) error
}

// Caps describes the limits of a target. A zero limit means "no limit",
// except MaxFiles, where 0 means the target sends text only.
type Caps struct {
	// MaxText is the longest text the target accepts, in the unit of
	// Measure.
	MaxText int
	// Measure returns the length of a text as the target counts it; nil
	// counts characters.
	Measure func(string) int
	// MaxFiles is the number of attachments sent with one message.
	MaxFiles int
	// MaxFileSize is the largest attachment in bytes.
	MaxFileSize int64
	// MaxFilesSize is the sum of the attachment sizes of one message in
	// bytes.
	MaxFilesSize int64
	// CanTakeMessage asks for Payload.Message: the target hands the
	// message itself on, as the exec target does on the stdin of its hook.
	CanTakeMessage bool
}

// Payload is what one target receives.
type Payload struct {
	// Title is the short headline of the notification: the subject, or
	// the notice for a missing one.
	Title string
	// Text is the notification in the markup of the target, within
	// Caps.MaxText. It is empty only when the target rejected the text
	// with IsTextRejected and the full text goes again as an attachment:
	// the target then sends the attachments alone.
	Text string
	// Attachments are the files to send, within the file limits of Caps.
	Attachments []Attachment
	// Message is set only for a target whose Caps.CanTakeMessage is set;
	// the target only reads it, as all targets share it.
	Message *Message
}

// Message is the message itself and its envelope, for a target that
// hands it on whole. Nothing in it names a blind copy.
type Message struct {
	// Raw is the message as it was read, without its Bcc and Resent-Bcc
	// fields; empty when the raw message is not known.
	Raw []byte
	// Subject is the decoded Subject; empty when the message has none.
	Subject string
	// From is the address of the From header, or the envelope sender.
	From string
	// To are the envelope recipients without the addresses that only a
	// Bcc or Resent-Bcc field named.
	To []string
	// MessageID is the Message-ID header.
	MessageID string
	// Hostname is the name of the machine.
	Hostname string
}

// Attachment is one file of a Payload.
type Attachment struct {
	// Name is the file name, never empty.
	Name string
	// ContentType is the media type; empty when unknown.
	ContentType string
	// Data is the content.
	Data []byte
}

// Class tells whether a failed delivery is worth retrying.
type Class uint8

const (
	// Temporary failures (rate limit, server error, timeout, network) may
	// succeed on a later attempt.
	Temporary Class = iota + 1
	// Permanent failures (rejected request) repeat on every attempt.
	Permanent
)

// String returns the lowercase name used in log fields.
func (c Class) String() string {
	switch c {
	case Temporary:
		return "temporary"
	case Permanent:
		return "permanent"
	default:
		return fmt.Sprintf("class(%d)", uint8(c))
	}
}

// Error is the only error type a Sender returns.
type Error struct {
	// Class decides whether the delivery is retried.
	Class Class
	// Status is the HTTP status code; 0 when the failure is not an HTTP
	// response.
	Status int
	// RetryAfter is the delay the service asked for; 0 when it did not ask.
	RetryAfter time.Duration
	// Err is the underlying cause. It must not contain secrets, because it
	// ends up in syslog.
	Err error
	// IsPartial reports that the text reached the target and only
	// attachments failed: sending the message again would repeat the
	// text, so the delivery counts as done.
	IsPartial bool
	// IsTextRejected reports that the service refused the request that
	// carries the text, as the Bot API does with 400 for markup it cannot
	// parse or a text over its limit, and nothing arrived: the full text
	// may still go as a file.
	IsTextRejected bool
}

// Error returns the class, the status when known, and the cause.
func (e *Error) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("%s failure, status %d: %v", e.Class, e.Status, e.Err)
	}
	return fmt.Sprintf("%s failure: %v", e.Class, e.Err)
}

// Unwrap returns the underlying cause.
func (e *Error) Unwrap() error {
	return e.Err
}
