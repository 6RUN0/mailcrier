// Package delivery sends one payload to every target and turns the
// per-target outcomes into the process exit status.
package delivery

import (
	"context"
	"errors"

	"github.com/6RUN0/slendmail/internal/backend"
)

// Exit statuses from sysexits.h that delivery can produce.
const (
	exitOK          = 0
	exitUnavailable = 69
)

// Target is a configured destination.
type Target struct {
	// ID is the target name from the configuration; it identifies the
	// target in logs.
	ID string
	// Sender performs the actual delivery.
	Sender backend.Sender
}

// Status is the outcome of one delivery attempt.
type Status uint8

const (
	// OK means the target accepted the payload.
	OK Status = iota + 1
	// Temp means the attempt failed and a later one may succeed.
	Temp
	// Perm means the target rejected the payload for good.
	Perm
	// Suppressed means a rule kept the payload from the target on purpose.
	Suppressed
)

// String returns the lowercase name used in log fields.
func (s Status) String() string {
	switch s {
	case OK:
		return "ok"
	case Temp:
		return "temp"
	case Perm:
		return "perm"
	case Suppressed:
		return "suppressed"
	default:
		return "unknown"
	}
}

// Result is the outcome for one target.
type Result struct {
	// TargetID is Target.ID of the target the result belongs to.
	TargetID string
	// Status is the outcome.
	Status Status
	// Err is the delivery error; nil when Status is OK.
	Err error
}

// Deliver sends p to every target in order and returns one Result per
// target, in the same order.
func Deliver(ctx context.Context, targets []Target, p backend.Payload) []Result {
	results := make([]Result, 0, len(targets))
	for _, target := range targets {
		err := target.Sender.Send(ctx, p)
		results = append(results, Result{TargetID: target.ID, Status: statusOf(err), Err: err})
	}
	return results
}

// statusOf treats an error that is not *backend.Error as permanent: it
// breaks the Sender contract, and retrying a bug does not fix it.
func statusOf(err error) Status {
	if err == nil {
		return OK
	}
	var deliveryErr *backend.Error
	if errors.As(err, &deliveryErr) && deliveryErr.Class == backend.Temporary {
		return Temp
	}
	return Perm
}

// ExitCode returns the process exit status for the results of one message
// when no spool is available to keep temporary failures.
//
// One accepted delivery makes the whole call a success: a non-zero status
// makes cron mail the failure report through this very program, which
// multiplies the noise without delivering anything. A message suppressed
// for every target is handled as intended and succeeds too. Without a
// spool a temporary failure is as final as a permanent one, so when
// nothing was delivered the status is 69 either way; 75 would claim the
// message was queued.
func ExitCode(results []Result) int {
	suppressed := 0
	for _, r := range results {
		switch r.Status {
		case OK:
			return exitOK
		case Suppressed:
			suppressed++
		}
	}
	if suppressed > 0 && suppressed == len(results) {
		return exitOK
	}
	return exitUnavailable
}
