// Package delivery renders a message for every target, sends it to all
// of them at once and turns the per-target outcomes into the process exit
// status.
package delivery

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"runtime/debug"
	"slices"
	"sync"
	"time"

	"github.com/6RUN0/mailcrier/internal/backend"
	"github.com/6RUN0/mailcrier/internal/message"
	"github.com/6RUN0/mailcrier/internal/render"
	"github.com/6RUN0/mailcrier/internal/text"
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
	// Template renders the text of the target; nil for a target that
	// sends no text, an http target with method GET.
	Template *render.Template
	// Fallback is the built-in template of the target when Template comes
	// from the configuration, nil otherwise. It renders the text when
	// Template fails, and once more when the target rejects the text of
	// Template.
	Fallback *render.Template
	// Request renders the path, query and headers of the request of an
	// http target; nil for any other target.
	Request *RequestTemplates
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

// RequestTemplates are the templates of the parts of an HTTP request, each
// from render.ParsePart. They have no built-in fallback: when one fails,
// the target fails permanently and gets nothing.
type RequestTemplates struct {
	// Path renders the path appended to the configured URL; nil for none.
	Path *render.Template
	// Query renders the query values by name.
	Query map[string]*render.Template
	// Headers render the header values by name.
	Headers map[string]*render.Template
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
	// TextRejected is the error of the attempt before the last when the
	// target rejected the text and the full text went again as a file
	// alone; nil otherwise.
	TextRejected error
	// TemplateErr is the failure of the template from the configuration,
	// a *render.TemplateError, or the error of the target that rejected
	// its text, when Fallback rendered the text instead; nil otherwise.
	TemplateErr error
	// RequestErr is the failure of a template of Target.Request; the
	// target then failed permanently without a request, and Err holds the
	// same failure. nil otherwise.
	RequestErr error
}

// IsInternal reports that the target failed by a panic, a bug of this
// program or of a library, rather than by an answer of the service.
func (r Result) IsInternal() bool {
	var panicErr *backend.PanicError
	return errors.As(r.Err, &panicErr)
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
// A target whose template from the configuration fails gets the text of
// its Fallback template, and so does a target that rejects the text of
// that template, once, before the full text goes as a file.
//
// A panic while rendering or sending for one target, a bug, becomes a
// permanent failure of that target with a *backend.PanicError, which
// Result.IsInternal reports: the others still deliver, and the value goes
// into the error, which the caller logs redacted.
func Deliver(ctx context.Context, targets []Target, d render.Data, files []message.Attachment) []Result {
	results, _ := DeliverEach(ctx, targets, d, files, nil, nil)
	return results
}

// DeliverEach is Deliver that also passes each result to done as soon as
// its target is finished, so that the caller can record it before the
// slowest target returns; done is called from several goroutines at once
// and may be nil. raw is the kept input of the message, message.Raw, for
// LongFileMessage.
//
// A panic of done would end the process from a goroutine of its own: it is
// recovered, the other calls of done go on, and donePanic returns the first
// one with the stack where it happened, beside the results of all targets,
// for the caller to record what done did not and to raise it again.
func DeliverEach(ctx context.Context, targets []Target, d render.Data, files []message.Attachment, raw []byte, done func(Result)) (results []Result, donePanic *backend.PanicError) {
	results = make([]Result, len(targets))
	long := newLongFiles(d, raw)
	var wg sync.WaitGroup
	var mu sync.Mutex
	for i, target := range targets {
		wg.Go(func() {
			results[i] = deliverOne(ctx, target, d, files, long)
			if done == nil {
				return
			}
			defer func() {
				if value := recover(); value != nil {
					mu.Lock()
					defer mu.Unlock()
					if donePanic == nil {
						donePanic = &backend.PanicError{Value: value, Stack: debug.Stack()}
					}
				}
			}()
			done(results[i])
		})
	}
	wg.Wait()
	return results, donePanic
}

// deliverOne renders and sends the message for one target.
func deliverOne(ctx context.Context, target Target, d render.Data, files []message.Attachment, long *longFiles) (result Result) {
	defer func() {
		if value := recover(); value != nil {
			result = Result{TargetID: target.ID, Status: Perm, Err: &backend.PanicError{Value: value, Stack: debug.Stack()}}
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
	job := &textJob{target: target, caps: caps, limit: limit, measure: measure, full: d, named: nameFiles(files), long: long}
	job.deadline, _ = ctx.Deadline()
	job.d = d
	job.d.Target, job.d.Limit = target.ID, limit
	var request *backend.Request
	if target.Request != nil {
		var err error
		if request, err = job.renderRequest(target.Request); err != nil {
			err = fmt.Errorf("render request: %w", err)
			return Result{TargetID: target.ID, Status: Perm, Err: &backend.Error{Class: backend.Permanent, Err: err}, RequestErr: err}
		}
	}
	out, err := job.fit(target.Template)
	var templateErr error
	var userErr *render.TemplateError
	if errors.As(err, &userErr) && target.Fallback != nil {
		templateErr = err
		out, err = job.fit(target.Fallback)
	}
	if err != nil {
		err = &backend.Error{Class: backend.Permanent, Err: fmt.Errorf("render: %w", err)}
		return Result{TargetID: target.ID, Status: Perm, Err: err, TemplateErr: templateErr}
	}
	title := d.Subject
	if title == "" {
		title = job.d.Strings.NoSubject
	}
	send := func(out *fitted) error {
		payload := backend.Payload{Title: title, Text: out.text, Attachments: out.sent, Request: request}
		if caps.CanTakeMessage {
			payload.Message = long.whole()
		}
		return target.Sender.Send(ctx, payload)
	}
	err = send(out)
	result = Result{TargetID: target.ID, Status: statusOf(err), Err: err, IsTruncated: out.isTruncated, TemplateErr: templateErr}
	var deliveryErr *backend.Error
	if !errors.As(err, &deliveryErr) || !deliveryErr.IsTextRejected {
		return result
	}
	if templateErr == nil && target.Fallback != nil {
		builtin, fitErr := job.fit(target.Fallback)
		if fitErr != nil {
			return result
		}
		out, templateErr = builtin, err
		err = send(out)
		result = Result{TargetID: target.ID, Status: statusOf(err), Err: err, IsTruncated: out.isTruncated, TemplateErr: templateErr}
		if !errors.As(err, &deliveryErr) || !deliveryErr.IsTextRejected {
			return result
		}
	}
	if caps.MaxFiles == 0 {
		return result
	}
	longFile := out.longFile
	if longFile == nil {
		if longFile, err = long.file(target.LongFile); err != nil {
			return result
		}
	}
	sent := out.sent
	if !out.isLongFileSent {
		listed, withFile := selectWithLongFile(caps, job.full.Attachments, job.named, longFile)
		if listed[0].IsSkipped {
			return result
		}
		sent = withFile
	}
	err = target.Sender.Send(ctx, backend.Payload{Title: title, Attachments: sent})
	return Result{TargetID: target.ID, Status: statusOf(err), Err: err, IsTruncated: out.isTruncated, TextRejected: result.Err, TemplateErr: templateErr}
}

// renderRequest renders the parts of the request with one budget, which
// the text then shares: j.deadline becomes the end of that budget.
func (j *textJob) renderRequest(parts *RequestTemplates) (*backend.Request, error) {
	request := &backend.Request{}
	execute := func(tmpl *render.Template) (string, error) {
		tmpl = tmpl.WithBudget(j.deadline)
		if deadline := tmpl.Deadline(); !deadline.IsZero() {
			j.deadline = deadline
		}
		return tmpl.Execute(j.d)
	}
	var err error
	if parts.Path != nil {
		if request.Path, err = execute(parts.Path); err != nil {
			return nil, fmt.Errorf("path: %w", err)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(parts.Query)) {
		value, err := execute(parts.Query[name])
		if err != nil {
			return nil, fmt.Errorf("query %q: %w", name, err)
		}
		if request.Query == nil {
			request.Query = url.Values{}
		}
		request.Query.Add(name, value)
	}
	for _, name := range slices.Sorted(maps.Keys(parts.Headers)) {
		value, err := execute(parts.Headers[name])
		if err != nil {
			return nil, fmt.Errorf("header %q: %w", name, err)
		}
		if request.Headers == nil {
			request.Headers = map[string]string{}
		}
		request.Headers[name] = value
	}
	return request, nil
}

// textJob is what rendering the text of one target needs, for each
// template it tries.
type textJob struct {
	target  Target
	caps    backend.Caps
	limit   int
	measure func(string) int
	// full is the data of the message; d the same with the target and its
	// limit set.
	full, d render.Data
	named   []backend.Attachment
	long    *longFiles
	// deadline is that of the delivery; zero for none.
	deadline time.Time
}

// fitted is the text of a target and the files that go with it.
type fitted struct {
	text        string
	isTruncated bool
	// sent are the files to send: the attachments within the limits of
	// the target, with longFile first when isLongFileSent.
	sent           []backend.Attachment
	longFile       *backend.Attachment
	isLongFileSent bool
}

// fit renders the text of the target with tmpl, fitted to its limit, and
// picks the files: for a cut text the full text goes first among them
// per OnLong, else the notice of the cut gives its size. render.ExecuteWhole
// tells a cut text before the files are picked, so the binary searches of
// render.FitLines run once, on the data with the files. A template from
// the configuration gets one budget for both, ending no later than the
// delivery.
func (j *textJob) fit(tmpl *render.Template) (*fitted, error) {
	if tmpl == nil {
		return &fitted{}, nil
	}
	tmpl = tmpl.WithBudget(j.deadline)
	d, caps := j.d, j.caps
	out := &fitted{}
	d.Attachments, out.sent = selectFiles(caps, j.full.Attachments, j.named)
	var err error
	out.text, out.isTruncated, err = render.ExecuteWhole(tmpl, d, j.limit, j.target.MaxLines, j.measure)
	if err != nil || !out.isTruncated {
		return out, err
	}
	if j.target.OnLong != OnLongTruncate && caps.MaxFiles > 0 {
		if out.longFile, err = j.long.file(j.target.LongFile); err != nil {
			return nil, err
		}
		listed, withFile := selectWithLongFile(caps, j.full.Attachments, j.named, out.longFile)
		if out.isLongFileSent = !listed[0].IsSkipped; out.isLongFileSent {
			d.Attachments, out.sent = listed, withFile
		}
	}
	if !out.isLongFileSent {
		size, err := j.long.textSize()
		if err != nil {
			return nil, err
		}
		d.Strings = d.Strings.WithFullSize(size)
	}
	d.IsCollapsed = j.target.OnLong == OnLongBlockquote
	out.text, _, err = render.FitLines(tmpl, d, j.limit, j.target.MaxLines, j.measure)
	return out, err
}

// fullTextTemplate is the built-in plain template, which renders the
// full text of a long message for every target.
var fullTextTemplate = sync.OnceValues(func() (*render.Template, error) {
	return render.Builtin(text.FormatPlain)
})

// longFiles builds the files that carry a long text in full, and the
// message for a target that takes it, once per message, on first use,
// for all targets: a copy of a message of 10 MiB per target would
// multiply the memory by the number of targets. The targets only read the
// bytes.
type longFiles struct {
	text    func() (*backend.Attachment, error)
	message func() *backend.Attachment
	whole   func() *backend.Message
}

// newLongFiles returns the long files of d, rendered without a limit,
// and of raw, the kept input of the message; nil raw has no message.eml.
// whole is the message for a target that takes it, with the bytes of
// message.eml.
func newLongFiles(d render.Data, raw []byte) *longFiles {
	l := &longFiles{
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
	l.whole = sync.OnceValue(func() *backend.Message {
		whole := &backend.Message{Subject: d.Subject, From: d.From.Addr, To: d.Recipients, MessageID: d.MessageID, Hostname: d.Hostname}
		if eml := l.message(); eml != nil {
			whole.Raw = eml.Data
		}
		return whole
	})
	return l
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
//   - one accepted delivery exits 0: a non-zero status makes cron mail
//     the failure report through this very program, which multiplies the
//     noise without delivering anything;
//   - a permanent failure exits 69;
//   - temporary failures kept in the spool exit 0, as a classic MTA does
//     for a queued message; 75 would make cron and smartd report a
//     failure;
//   - temporary failures without a spool exit 69 like permanent ones.
func ExitCode(results []Result, queue Queue) int {
	var ok, temp, perm int
	for _, r := range results {
		switch r.Status {
		case OK:
			ok++
		case Temp:
			temp++
		case Perm:
			perm++
		}
	}
	switch {
	case temp > 0 && queue == QueueNotCreated:
		return exitCantCreate
	case temp > 0 && queue == QueueNotWritten:
		return exitIOErr
	case ok > 0:
		return exitOK
	case perm > 0:
		return exitUnavailable
	case temp > 0 && queue == Queued:
		return exitOK
	default:
		return exitUnavailable
	}
}
