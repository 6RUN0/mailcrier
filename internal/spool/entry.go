package spool

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/6RUN0/slendmail/internal/message"
)

// Version is the sidecar format this package writes and reads.
const Version = 1

// Retry schedule: the delay after the n-th failed attempt is
// BaseDelay x 2^(n-1), at most MaxDelay.
const (
	BaseDelay = 60 * time.Second
	MaxDelay  = 24 * time.Hour
)

// maxErrorLength bounds LastError: an error text may quote a response,
// and the sidecar is rewritten after every attempt.
const maxErrorLength = 512

// State is the delivery state of one target of an entry.
type State string

const (
	// Pending means the target still has to get the message.
	Pending State = "pending"
	// Done means the target accepted the message, or a rule suppressed it.
	Done State = "done"
	// Failed means the target will not get the message: it rejected it
	// for good, or it is gone from the configuration.
	Failed State = "failed"
)

// Errors of Decode.
var (
	// ErrUnknownVersion marks a sidecar written in a format this build
	// does not know, by a newer or an older release; it is left alone.
	ErrUnknownVersion = errors.New("unknown sidecar version")
	// ErrCorrupt marks a sidecar that is not a valid entry.
	ErrCorrupt = errors.New("corrupt sidecar")
)

// Entry is the sidecar of a spooled message: who queued it, where it
// goes and how far its delivery got. The message itself is stored beside
// it unchanged.
type Entry struct {
	// Version is the format of the sidecar.
	Version int `json:"version"`
	// ID names the files of the entry.
	ID string `json:"id"`
	// OwnerUID is the real uid of the process that queued the message;
	// only that user, root and the service user run the entry.
	OwnerUID int `json:"owner_uid"`
	// CreatedAt is when the entry was written; the TTL of hold/, and of
	// queue/ for an entry never held, counts from it.
	CreatedAt time.Time `json:"created_at"`
	// ReleasedAt is when the entry left hold/ for the queue, from which
	// the TTL of queue/ counts; zero for an entry queued directly.
	ReleasedAt time.Time `json:"released_at,omitzero"`
	// ReceivedAt is when the message was read; a later rendering uses it
	// for a message without a Date header.
	ReceivedAt time.Time `json:"received_at"`
	// Envelope is the envelope computed when the message was received.
	Envelope message.Envelope `json:"envelope"`
	// Targets maps the target name to its state. The set is fixed when
	// the message is queued; a held message has none until it is
	// released.
	Targets map[string]*TargetState `json:"targets,omitempty"`
	// Reason says why the entry is held or failed.
	Reason string `json:"reason,omitempty"`
	// FailedAt is when the entry moved to failed/; failed_ttl counts
	// from it.
	FailedAt time.Time `json:"failed_at,omitzero"`
}

// TargetState is the delivery state of one target.
type TargetState struct {
	// State is where the delivery to the target stands.
	State State `json:"state"`
	// Attempts counts the attempts that failed temporarily.
	Attempts int `json:"attempts,omitempty"`
	// NextAt is the earliest time of the next attempt.
	NextAt time.Time `json:"next_at,omitzero"`
	// LastClass is "temp" or "perm" after a failure.
	LastClass string `json:"last_class,omitempty"`
	// LastError is the text of the last failure, redacted by the caller.
	LastError string `json:"last_error,omitempty"`
}

// validID matches NewID: nanoseconds, a dash and 16 hex digits.
var validID = regexp.MustCompile(`^[0-9]{1,20}-[0-9a-f]{16}$`)

// NewID returns an entry id: the time in nanoseconds, so that names sort
// by age, and 8 random bytes, so that processes started in the same
// nanosecond do not collide.
func NewID(now time.Time) string {
	suffix := make([]byte, 8)
	_, _ = rand.Read(suffix) // never fails on Linux, per crypto/rand
	return strconv.FormatInt(now.UnixNano(), 10) + "-" + hex.EncodeToString(suffix)
}

// NewEntry returns an entry of Version for a message received at
// receivedAt and queued at now, with every target in targets pending.
func NewEntry(id string, ownerUID int, now, receivedAt time.Time, env message.Envelope, targets []string) *Entry {
	e := &Entry{Version: Version, ID: id, OwnerUID: ownerUID, CreatedAt: now, ReceivedAt: receivedAt, Envelope: env}
	e.SetTargets(targets)
	return e
}

// SetTargets replaces the targets with targets, all pending and due.
func (e *Entry) SetTargets(targets []string) {
	e.Targets = nil
	if len(targets) == 0 {
		return
	}
	e.Targets = make(map[string]*TargetState, len(targets))
	for _, name := range targets {
		e.Targets[name] = &TargetState{State: Pending}
	}
}

// IsPending reports whether any target still has to get the message.
func (e *Entry) IsPending() bool {
	for _, target := range e.Targets {
		if target.State == Pending {
			return true
		}
	}
	return false
}

// MarkDone records that target accepted the message.
func (e *Entry) MarkDone(target string) {
	e.Targets[target] = &TargetState{State: Done, Attempts: e.attempts(target)}
}

// MarkFailed records that target will not get the message; class and
// text describe why.
func (e *Entry) MarkFailed(target, class, text string) {
	e.Targets[target] = &TargetState{State: Failed, Attempts: e.attempts(target), LastClass: class, LastError: cut(text)}
}

// MarkRetry records a temporary failure of target at now and schedules
// the next attempt with NextAttempt.
func (e *Entry) MarkRetry(target string, now time.Time, retryAfter time.Duration, class, text string) {
	attempts := e.attempts(target) + 1
	e.Targets[target] = &TargetState{
		State: Pending, Attempts: attempts, NextAt: NextAttempt(now, attempts, retryAfter),
		LastClass: class, LastError: cut(text),
	}
}

func (e *Entry) attempts(target string) int {
	if state, ok := e.Targets[target]; ok {
		return state.Attempts
	}
	return 0
}

// cut bounds an error text to maxErrorLength bytes of valid UTF-8.
func cut(text string) string {
	if len(text) <= maxErrorLength {
		return text
	}
	return string(bytes.ToValidUTF8([]byte(text[:maxErrorLength]), nil))
}

// NextAttempt returns the time of the next attempt after the attempts-th
// failure at now: BaseDelay doubled with each failure after the first, at
// most MaxDelay, and never before the delay the service asked for.
func NextAttempt(now time.Time, attempts int, retryAfter time.Duration) time.Time {
	delay := MaxDelay
	if attempts < 1 {
		attempts = 1
	}
	// 60 s x 2^11 exceeds 24 h, so the shift stops well before overflow.
	if shift := attempts - 1; shift < 11 {
		delay = min(BaseDelay<<shift, MaxDelay)
	}
	return now.Add(max(delay, retryAfter))
}

// Expired reports whether e has outlived ttl at now, counted from the
// time it entered its current area: ReleasedAt when set, else CreatedAt.
func Expired(e *Entry, now time.Time, ttl time.Duration) bool {
	since := e.CreatedAt
	if e.ReleasedAt.After(since) {
		since = e.ReleasedAt
	}
	return now.Sub(since) > ttl
}

// Encode returns the sidecar of e.
func Encode(e *Entry) ([]byte, error) {
	return json.MarshalIndent(e, "", "\t")
}

// Decode parses a sidecar. A sidecar of another Version yields an error
// wrapping ErrUnknownVersion without looking further, so that a format
// change never makes an older build rewrite what it does not understand;
// any other defect wraps ErrCorrupt.
func Decode(data []byte) (*Entry, error) {
	var head struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrCorrupt, err)
	}
	if head.Version != Version {
		return nil, fmt.Errorf("%w: %d", ErrUnknownVersion, head.Version)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var e Entry
	if err := dec.Decode(&e); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrCorrupt, err)
	}
	if dec.More() {
		return nil, fmt.Errorf("%w: data after the entry", ErrCorrupt)
	}
	if !validID.MatchString(e.ID) {
		return nil, fmt.Errorf("%w: invalid id", ErrCorrupt)
	}
	for name, target := range e.Targets {
		if target == nil {
			return nil, fmt.Errorf("%w: target %q has no state", ErrCorrupt, name)
		}
		switch target.State {
		case Pending, Done, Failed:
		default:
			return nil, fmt.Errorf("%w: target %q has an unknown state", ErrCorrupt, name)
		}
	}
	return &e, nil
}
