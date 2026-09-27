// Package delivery renders a message for every target, sends it to all
// of them at once and turns the per-target outcomes into the process exit
// status.
package delivery

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/6RUN0/slendmail/internal/backend"
	"github.com/6RUN0/slendmail/internal/message"
	"github.com/6RUN0/slendmail/internal/render"
	"github.com/6RUN0/slendmail/internal/text"
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
	// Template renders the text of the target.
	Template *render.Template
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
	// Err is the delivery error. With Status OK it is nil, or an error
	// whose IsPartial is set: the text arrived, attachments did not.
	Err error
	// IsTruncated reports that the text was cut to the length limit of
	// the target.
	IsTruncated bool
}

// Deliver renders d for every target, fitted to its length limit, and
// sends it with the files of the message, all targets at once. files are
// the attachments of the message in the order of d.Attachments. The
// results come in the order of targets.
//
// A panic while rendering or sending for one target, a bug, becomes a
// permanent failure of that target: the others still deliver, and the
// value goes into the error, which the caller logs redacted.
func Deliver(ctx context.Context, targets []Target, d render.Data, files []message.Attachment) []Result {
	results := make([]Result, len(targets))
	var wg sync.WaitGroup
	for i, target := range targets {
		wg.Go(func() {
			results[i] = deliverOne(ctx, target, d, files)
		})
	}
	wg.Wait()
	return results
}

// deliverOne renders and sends the message for one target.
func deliverOne(ctx context.Context, target Target, d render.Data, files []message.Attachment) (result Result) {
	defer func() {
		if value := recover(); value != nil {
			result = Result{TargetID: target.ID, Status: Perm, Err: fmt.Errorf("panic: %v", value)}
		}
	}()
	caps := target.Sender.Caps()
	measure := caps.Measure
	if measure == nil {
		measure = text.RuneCount
	}
	d.Target, d.Limit = target.ID, caps.MaxText
	var sent []backend.Attachment
	d.Attachments, sent = selectFiles(caps, d.Attachments, files)
	out, isTruncated, err := render.Fit(target.Template, d, caps.MaxText, measure)
	if err != nil {
		err = &backend.Error{Class: backend.Permanent, Err: fmt.Errorf("render: %w", err)}
		return Result{TargetID: target.ID, Status: Perm, Err: err}
	}
	title := d.Subject
	if title == "" {
		title = d.Strings.NoSubject
	}
	err = target.Sender.Send(ctx, backend.Payload{Title: title, Text: out, Attachments: sent})
	return Result{TargetID: target.ID, Status: statusOf(err), Err: err, IsTruncated: isTruncated}
}

// selectFiles returns a copy of listed with the attachments over the file
// limits of caps marked as skipped, and the files to send, in order. A
// target without file support gets no file and no mark: its text lists
// the attachments as a plain inventory. A file without a name gets
// "attachment-<n>", n counting from 1, because every file API wants one,
// and the list in the text gets the same name, for every target, so that
// the reader can tell which line the file belongs to.
func selectFiles(caps backend.Caps, listed []render.Attachment, files []message.Attachment) ([]render.Attachment, []backend.Attachment) {
	marked := make([]render.Attachment, len(listed))
	copy(marked, listed)
	var sent []backend.Attachment
	var total int64
	for i, file := range files {
		size := int64(len(file.Data))
		fits := len(sent) < caps.MaxFiles &&
			(caps.MaxFileSize == 0 || size <= caps.MaxFileSize) &&
			(caps.MaxFilesSize == 0 || total+size <= caps.MaxFilesSize)
		name := file.Name
		if name == "" {
			name = fmt.Sprintf("attachment-%d", i+1)
		}
		if i < len(marked) {
			marked[i].Name, marked[i].IsSkipped = name, !fits && caps.MaxFiles > 0
		}
		if !fits {
			continue
		}
		sent = append(sent, backend.Attachment{Name: name, ContentType: file.ContentType, Data: file.Data})
		total += size
	}
	return marked, sent
}

// statusOf treats an error that is not *backend.Error as permanent: it
// breaks the Sender contract, and retrying a bug does not fix it. A
// partial failure is OK: the text arrived, and a retry would repeat it.
func statusOf(err error) Status {
	if err == nil {
		return OK
	}
	var deliveryErr *backend.Error
	if !errors.As(err, &deliveryErr) {
		return Perm
	}
	switch {
	case deliveryErr.IsPartial:
		return OK
	case deliveryErr.Class == backend.Temporary:
		return Temp
	default:
		return Perm
	}
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
