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
	exitCantCreate  = 73
	exitIOErr       = 74
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
	// OnLong is what the target gets for a text over its limit.
	OnLong OnLong
	// LongFile selects the file that carries a long text in full.
	LongFile LongFile
	// MaxText replaces Caps.MaxText of the sender when positive, in the
	// unit of Caps.Measure.
	MaxText int
	// MaxLines bounds the lines of the body in the text when positive; a
	// body of more lines counts as a text over the limit.
	MaxLines int
	// MaxFileSize replaces Caps.MaxFileSize of the sender when positive,
	// in bytes.
	MaxFileSize int64
}

// OnLong is the policy for a text over the limit of a target. Every
// policy cuts the text to the limit; they differ in what goes along.
type OnLong uint8

const (
	// OnLongFile sends the full text as a file too, when the target takes
	// files; a target without files gets the cut text with the size of
	// the full text in the notice.
	OnLongFile OnLong = iota
	// OnLongTruncate sends the cut text alone, with the size of the full
	// text in the notice.
	OnLongTruncate
	// OnLongBlockquote is OnLongFile with the cut body in a collapsed
	// block, where the markup of the target has one: an expandable
	// blockquote in Telegram.
	OnLongBlockquote
)

// LongFile is the file that carries a long text in full.
type LongFile uint8

const (
	// LongFileText is message.txt, the message in the plain built-in
	// template without a length limit.
	LongFileText LongFile = iota
	// LongFileMessage is message.eml, the message as it was read, without
	// its Bcc and Resent-Bcc fields; LongFileText when the raw message is
	// not known.
	LongFileMessage
)

// Names of the files that carry a long text in full.
const (
	textFileName    = "message.txt"
	messageFileName = "message.eml"
)

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
	// TextRejected is the error of the first attempt when the target
	// rejected the text and the full text went again as a file alone; nil
	// otherwise.
	TextRejected error
}

// Deliver renders d for every target, fitted to its length limit, and
// sends it with the files of the message, all targets at once. files are
// the attachments of the message in the order of d.Attachments. The
// results come in the order of targets.
//
// A text over the limit of a target is cut, and with OnLongFile or
// OnLongBlockquote the full text goes as the first file, ahead of the
// attachments, when the target takes files. A target that rejects the
// text, backend.Error.IsTextRejected, gets the full text as a file alone,
// once, whatever its OnLong.
//
// A panic while rendering or sending for one target, a bug, becomes a
// permanent failure of that target: the others still deliver, and the
// value goes into the error, which the caller logs redacted.
func Deliver(ctx context.Context, targets []Target, d render.Data, files []message.Attachment) []Result {
	return DeliverEach(ctx, targets, d, files, nil, nil)
}

// DeliverEach is Deliver that also passes each result to done as soon as
// its target is finished, so that the caller can record it before the
// slowest target returns; done is called from several goroutines at once
// and may be nil. raw is the kept input of the message, message.Raw, for
// LongFileMessage.
func DeliverEach(ctx context.Context, targets []Target, d render.Data, files []message.Attachment, raw []byte, done func(Result)) []Result {
	results := make([]Result, len(targets))
	long := newLongFiles(d, raw)
	var wg sync.WaitGroup
	for i, target := range targets {
		wg.Go(func() {
			results[i] = deliverOne(ctx, target, d, files, long)
			if done != nil {
				done(results[i])
			}
		})
	}
	wg.Wait()
	return results
}

// deliverOne renders and sends the message for one target.
func deliverOne(ctx context.Context, target Target, d render.Data, files []message.Attachment, long *longFiles) (result Result) {
	defer func() {
		if value := recover(); value != nil {
			result = Result{TargetID: target.ID, Status: Perm, Err: fmt.Errorf("panic: %v", value)}
		}
	}()
	caps := target.Sender.Caps()
	if target.MaxFileSize > 0 {
		caps.MaxFileSize = target.MaxFileSize
	}
	limit := caps.MaxText
	if target.MaxText > 0 {
		limit = target.MaxText
	}
	measure := caps.Measure
	if measure == nil {
		measure = text.RuneCount
	}
	full := d
	d.Target, d.Limit = target.ID, limit
	named := nameFiles(files)
	var sent []backend.Attachment
	d.Attachments, sent = selectFiles(caps, full.Attachments, named)
	out, isTruncated, err := render.FitLines(target.Template, d, limit, target.MaxLines, measure)
	var longFile *backend.Attachment
	isLongFileSent := false
	if err == nil && isTruncated {
		if target.OnLong != OnLongTruncate && caps.MaxFiles > 0 {
			if longFile, err = long.file(target.LongFile); err != nil {
				return Result{TargetID: target.ID, Status: Perm, Err: &backend.Error{Class: backend.Permanent, Err: fmt.Errorf("render: %w", err)}}
			}
			listed, withFile := selectWithLongFile(caps, full.Attachments, named, longFile)
			if isLongFileSent = !listed[0].IsSkipped; isLongFileSent {
				d.Attachments, sent = listed, withFile
			}
		}
		if !isLongFileSent {
			size, sizeErr := long.textSize()
			if sizeErr != nil {
				return Result{TargetID: target.ID, Status: Perm, Err: &backend.Error{Class: backend.Permanent, Err: fmt.Errorf("render: %w", sizeErr)}}
			}
			d.Strings = d.Strings.WithFullSize(size)
		}
		d.IsCollapsed = target.OnLong == OnLongBlockquote
		out, _, err = render.FitLines(target.Template, d, limit, target.MaxLines, measure)
	}
	if err != nil {
		err = &backend.Error{Class: backend.Permanent, Err: fmt.Errorf("render: %w", err)}
		return Result{TargetID: target.ID, Status: Perm, Err: err}
	}
	title := d.Subject
	if title == "" {
		title = d.Strings.NoSubject
	}
	err = target.Sender.Send(ctx, backend.Payload{Title: title, Text: out, Attachments: sent})
	result = Result{TargetID: target.ID, Status: statusOf(err), Err: err, IsTruncated: isTruncated}
	var deliveryErr *backend.Error
	if !errors.As(err, &deliveryErr) || !deliveryErr.IsTextRejected || caps.MaxFiles == 0 {
		return result
	}
	if longFile == nil {
		if longFile, err = long.file(target.LongFile); err != nil {
			return result
		}
	}
	if !isLongFileSent {
		listed, withFile := selectWithLongFile(caps, full.Attachments, named, longFile)
		if listed[0].IsSkipped {
			return result
		}
		sent = withFile
	}
	err = target.Sender.Send(ctx, backend.Payload{Title: title, Attachments: sent})
	return Result{TargetID: target.ID, Status: statusOf(err), Err: err, IsTruncated: isTruncated, TextRejected: result.Err}
}

// fullTextTemplate is the built-in plain template, which renders the
// full text of a long message for every target.
var fullTextTemplate = sync.OnceValues(func() (*render.Template, error) {
	return render.Builtin(text.FormatPlain)
})

// longFiles builds the files that carry a long text in full once per
// message, on first use, for all targets: a copy of a message of 10 MiB
// per target would multiply the memory by the number of targets. The
// targets only read the bytes.
type longFiles struct {
	text    func() (*backend.Attachment, error)
	message func() *backend.Attachment
}

// newLongFiles returns the long files of d, rendered without a limit,
// and of raw, the kept input of the message; nil raw has no message.eml.
func newLongFiles(d render.Data, raw []byte) *longFiles {
	return &longFiles{
		text: sync.OnceValues(func() (*backend.Attachment, error) {
			tmpl, err := fullTextTemplate()
			if err != nil {
				return nil, err
			}
			plain, err := tmpl.ExecuteBytes(d)
			if err != nil {
				return nil, err
			}
			return &backend.Attachment{Name: textFileName, ContentType: "text/plain; charset=utf-8", Data: plain}, nil
		}),
		message: sync.OnceValue(func() *backend.Attachment {
			if len(raw) == 0 {
				return nil
			}
			return &backend.Attachment{Name: messageFileName, ContentType: "message/rfc822", Data: message.WithoutBlindCopies(raw)}
		}),
	}
}

// file returns the file of kind; message.txt for LongFileMessage without
// a raw message. message.eml leaves message.txt unbuilt.
func (l *longFiles) file(kind LongFile) (*backend.Attachment, error) {
	if kind == LongFileMessage {
		if eml := l.message(); eml != nil {
			return eml, nil
		}
	}
	return l.text()
}

// textSize returns the size of message.txt in bytes, the full text that
// the notice of a text cut without its file names.
func (l *longFiles) textSize() (int64, error) {
	plain, err := l.text()
	if err != nil {
		return 0, err
	}
	return int64(len(plain.Data)), nil
}

// selectWithLongFile is selectFiles with file ahead of the attachments,
// listed as the first attachment of the text.
func selectWithLongFile(caps backend.Caps, listed []render.Attachment, files []backend.Attachment, file *backend.Attachment) ([]render.Attachment, []backend.Attachment) {
	listing := render.Attachment{Name: file.Name, ContentType: file.ContentType, Size: int64(len(file.Data))}
	return selectFiles(caps, append([]render.Attachment{listing}, listed...), append([]backend.Attachment{*file}, files...))
}

// nameFiles returns the attachments as backend files. A file without a
// name gets "attachment-<n>", n counting from 1, because every file API
// wants one; selectFiles gives the list in the text the same name, for
// every target, so that the reader can tell which line the file belongs
// to.
func nameFiles(files []message.Attachment) []backend.Attachment {
	named := make([]backend.Attachment, len(files))
	for i, file := range files {
		name := file.Name
		if name == "" {
			name = fmt.Sprintf("attachment-%d", i+1)
		}
		named[i] = backend.Attachment{Name: name, ContentType: file.ContentType, Data: file.Data}
	}
	return named
}

// selectFiles returns a copy of listed with the names of files and the
// files over the file limits of caps marked as skipped, and the files to
// send, in order. A target without file support gets no file and no mark:
// its text lists the attachments as a plain inventory.
func selectFiles(caps backend.Caps, listed []render.Attachment, files []backend.Attachment) ([]render.Attachment, []backend.Attachment) {
	marked := make([]render.Attachment, len(listed))
	copy(marked, listed)
	var sent []backend.Attachment
	var total int64
	for i, file := range files {
		size := int64(len(file.Data))
		fits := len(sent) < caps.MaxFiles &&
			(caps.MaxFileSize == 0 || size <= caps.MaxFileSize) &&
			(caps.MaxFilesSize == 0 || total+size <= caps.MaxFilesSize)
		if i < len(marked) {
			marked[i].Name, marked[i].IsSkipped = file.Name, !fits && caps.MaxFiles > 0
		}
		if !fits {
			continue
		}
		sent = append(sent, file)
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

// Queue is what happened to the spool entry of a message.
type Queue uint8

const (
	// QueueOff means the spool is turned off.
	QueueOff Queue = iota
	// Queued means the entry was written before the delivery, so a target
	// that failed temporarily gets the message on a later run.
	Queued
	// QueueNotCreated means no entry file could be created: the spool
	// directory is missing or not writable, or a quota is exhausted.
	QueueNotCreated
	// QueueNotWritten means writing, syncing or renaming the entry failed.
	QueueNotWritten
)

// ExitCode returns the process exit status for the results of one message
// and what happened to its spool entry. The rules apply in order:
//
//   - a temporary failure without an entry exits 73 when the entry could
//     not be created and 74 when it could not be written: the message is
//     lost for that target, which the caller must learn;
//   - one accepted delivery, or suppression for every target, exits 0: a
//     non-zero status makes cron mail the failure report through this
//     very program, which multiplies the noise without delivering
//     anything;
//   - a permanent failure exits 69;
//   - temporary failures kept in the spool exit 0, as a classic MTA does
//     for a queued message; 75 would make cron and smartd report a
//     failure;
//   - temporary failures without a spool exit 69 like permanent ones.
func ExitCode(results []Result, queue Queue) int {
	var ok, temp, perm, suppressed int
	for _, r := range results {
		switch r.Status {
		case OK:
			ok++
		case Temp:
			temp++
		case Perm:
			perm++
		case Suppressed:
			suppressed++
		}
	}
	switch {
	case temp > 0 && queue == QueueNotCreated:
		return exitCantCreate
	case temp > 0 && queue == QueueNotWritten:
		return exitIOErr
	case ok > 0, suppressed > 0 && suppressed == len(results):
		return exitOK
	case perm > 0:
		return exitUnavailable
	case temp > 0 && queue == Queued:
		return exitOK
	default:
		return exitUnavailable
	}
}
