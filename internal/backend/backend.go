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

// Caps describes the limits of a target. A zero value means "no limit".
type Caps struct {
	// MaxText is the longest text the target accepts, in the target's own
	// unit of length.
	MaxText int
}

// Payload is the text prepared for one target.
type Payload struct {
	// Title is the short headline of the notification, usually the subject.
	Title string
	// Text is the notification body.
	Text string
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
