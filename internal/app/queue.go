package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/6RUN0/slendmail/internal/backend"
	"github.com/6RUN0/slendmail/internal/config"
	"github.com/6RUN0/slendmail/internal/delivery"
	"github.com/6RUN0/slendmail/internal/message"
	"github.com/6RUN0/slendmail/internal/redact"
	"github.com/6RUN0/slendmail/internal/render"
	"github.com/6RUN0/slendmail/internal/spool"
)

// staleAge is the age after which a file in tmp/ is the rest of a process
// that died while writing it: writing an entry takes milliseconds.
const staleAge = time.Hour

// Reasons stored in the sidecar and logged when an entry leaves the queue
// without delivery.
const (
	reasonConfig        = "configuration rejected"
	reasonExpired       = "expired"
	reasonTargetRemoved = "target removed"
)

// queue delivers messages through the spool: the own message of a call
// and the entries of a queue run. A nil sp means the spool is off.
type queue struct {
	d        Deps
	log      *slog.Logger
	redactor *redact.Redactor
	settings config.Spool
	sp       *spool.Spool
	// targets are the configured targets by name; nil when the
	// configuration was rejected, which leaves a queue run only the
	// expiry of entries.
	targets map[string]delivery.Target
	// deadline bounds the delivery of one message; notices fill the
	// template data.
	deadline time.Duration
	notices  render.Strings

	mu sync.Mutex
	// throttled holds, per target, the time before which a queue run
	// sends it nothing more: the service asked for the delay.
	throttled map[string]time.Time
	// hasIOError records a spool error that makes -q exit 74.
	hasIOError bool
	// otherHold is another spool directory whose hold/ a queue run
	// releases into this queue; empty for none.
	otherHold string
}

// newQueue returns the queue of the spool settings with targets, which may
// be nil. When the directory cannot be opened the queue works without the
// spool, and openErr tells the caller why.
func newQueue(d Deps, log *slog.Logger, redactor *redact.Redactor, settings config.Spool, targets []delivery.Target) (q *queue, openErr error) {
	q = &queue{d: d, log: log, redactor: redactor, settings: settings, throttled: map[string]time.Time{}}
	if targets != nil {
		q.targets = map[string]delivery.Target{}
		for _, target := range targets {
			q.targets[target.ID] = target
		}
	}
	if settings.Dir == "" {
		return q, nil
	}
	sp, err := spool.Open(settings.Dir)
	if err != nil {
		return q, err
	}
	q.sp = sp
	return q, nil
}

// spoolSettings returns the [spool] settings of cfg, with the directory of
// d when the file does not set one; cfg nil means the file was rejected.
func spoolSettings(d Deps, cfg *config.Config) config.Spool {
	settings := config.DefaultSpool()
	if cfg != nil {
		settings = cfg.Spool
	}
	if cfg == nil || !cfg.Spool.HasDir {
		settings.Dir = d.SpoolDir
	}
	return settings
}

// quota returns the limits a new entry of owner must fit in.
func (q *queue) quota(owner int) spool.Quota {
	s := q.settings
	return spool.Quota{
		Limits: spool.Limits{
			Messages: s.MaxMessages, Bytes: s.MaxBytes, MessagesPerUID: s.MaxMessagesPerUID, BytesPerUID: s.MaxBytesPerUID,
		},
		IsReserved: owner == 0 || owner == q.d.Credentials.ServiceUID,
	}
}

// deliverOwn writes the message ahead into the queue, delivers it to all
// targets and returns the exit status. Without an entry the message is
// delivered all the same, and a temporary failure then exits 73 or 74.
func (q *queue) deliverOwn(ctx context.Context, targets []delivery.Target, msg *message.Message, env message.Envelope, data render.Data, openErr error) int {
	state := delivery.QueueOff
	var rec *spool.Record
	var spoolErr error
	if q.settings.Dir != "" {
		spoolErr = openErr
		if spoolErr == nil {
			names := make([]string, len(targets))
			for i, target := range targets {
				names[i] = target.ID
			}
			entry := spool.NewEntry(spool.NewID(q.d.Now()), q.d.Credentials.UID, q.d.Now(), data.ReceivedAt, env, names)
			rec, spoolErr = q.sp.Create(spool.QueueDir, entry, msg.Raw, q.quota(entry.OwnerUID))
		}
		switch {
		case spoolErr == nil:
			state = delivery.Queued
			q.log.Debug("message queued ahead of delivery", "id", rec.ID())
		case errors.Is(spoolErr, spool.ErrWrite):
			state = delivery.QueueNotWritten
		default:
			state = delivery.QueueNotCreated
		}
	}
	results := q.send(ctx, targets, env, data, msg.Attachments, msg.Raw, rec)
	for _, r := range results {
		logResult(q.log, r)
	}
	if rec != nil {
		q.finish(rec)
	}
	if spoolErr != nil {
		// Without a temporary failure nothing needed the entry.
		level := slog.LevelWarn
		if slices.ContainsFunc(results, func(r delivery.Result) bool { return r.Status == delivery.Temp }) {
			level = slog.LevelError
		}
		q.log.Log(ctx, level, "spool entry not written", "err", spoolErr)
	}
	logOutcome(q.log, results, state)
	return delivery.ExitCode(results, state)
}

// send delivers to targets and records each result in rec, when not nil,
// as soon as the target is finished. raw is the kept input of the
// message, for a target that sends it as a file.
func (q *queue) send(ctx context.Context, targets []delivery.Target, env message.Envelope, data render.Data, files []message.Attachment, raw []byte, rec *spool.Record) []delivery.Result {
	done := func(r delivery.Result) {
		q.mu.Lock()
		defer q.mu.Unlock()
		q.throttle(r)
		if rec == nil {
			return
		}
		q.apply(rec.Entry, r)
		if err := rec.Save(); err != nil {
			q.hasIOError = true
			q.log.Error("spool entry not updated", "id", rec.ID(), "target", r.TargetID, "err", err)
			return
		}
		if q.d.spoolSaved != nil {
			q.d.spoolSaved(rec.Entry)
		}
	}
	if q.d.deliver == nil {
		return delivery.DeliverEach(ctx, targets, data, files, raw, done)
	}
	results := q.d.deliver(ctx, targets, env, data, files)
	for _, r := range results {
		done(r)
	}
	return results
}

// apply records the result of one target in e.
func (q *queue) apply(e *spool.Entry, r delivery.Result) {
	errText := ""
	if r.Err != nil {
		errText = q.redactor.String(r.Err.Error())
	}
	switch r.Status {
	case delivery.OK, delivery.Suppressed:
		e.MarkDone(r.TargetID)
	case delivery.Temp:
		e.MarkRetry(r.TargetID, q.d.Now(), retryAfter(r.Err), delivery.Temp.String(), errText)
	default:
		e.MarkFailed(r.TargetID, delivery.Perm.String(), errText)
	}
}

// throttle keeps the target out of the rest of a queue run when the
// service asked for a delay, as a 429 does.
func (q *queue) throttle(r delivery.Result) {
	if delay := retryAfter(r.Err); r.Status == delivery.Temp && delay > 0 {
		q.throttled[r.TargetID] = q.d.Now().Add(delay)
	}
}

// retryAfter returns the delay a service asked for in err, 0 when none.
func retryAfter(err error) time.Duration {
	var deliveryErr *backend.Error
	if errors.As(err, &deliveryErr) {
		return deliveryErr.RetryAfter
	}
	return 0
}

// finish removes an entry that no target waits for and releases it.
func (q *queue) finish(rec *spool.Record) {
	if !rec.Entry.IsPending() {
		if err := rec.Remove(); err != nil {
			q.hasIOError = true
			q.log.Error("spool entry not removed", "id", rec.ID(), "err", err)
		}
	}
	_ = rec.Close()
}

// hold puts a message whose configuration was rejected into hold/, where
// a queue run takes it once the configuration loads again.
func (q *queue) hold(msg *message.Message, env message.Envelope, receivedAt time.Time, openErr error) {
	if q.settings.Dir == "" {
		q.log.Error("message lost, spool off")
		return
	}
	err := openErr
	if err == nil {
		entry := spool.NewEntry(spool.NewID(q.d.Now()), q.d.Credentials.UID, q.d.Now(), receivedAt, env, nil)
		entry.Reason = reasonConfig
		var rec *spool.Record
		if rec, err = q.sp.Create(spool.HoldDir, entry, msg.Raw, q.quota(entry.OwnerUID)); err == nil {
			q.log.Warn("message held", "id", rec.ID())
			_ = rec.Close()
			return
		}
	}
	q.log.Error("message lost, not held", "err", err)
}

// runLimits bound one queue run.
type runLimits struct {
	// owner restricts the run to the entries of one uid; -1 takes all.
	owner int
	// budget bounds the run; maxMessages bounds the entries it delivers,
	// 0 for no bound.
	budget      time.Duration
	maxMessages int
}

// run works through hold/ and queue/, oldest entries first, and returns
// an error when the spool cannot be listed. Entries locked by another
// process are skipped: that process delivers them.
func (q *queue) run(ctx context.Context, limits runLimits) error {
	ctx, cancel := context.WithTimeout(ctx, limits.budget)
	defer cancel()
	start := q.d.Now()
	delivered := 0
	isOver := func() bool {
		return ctx.Err() != nil || q.d.Now().Sub(start) >= limits.budget ||
			(limits.maxMessages > 0 && delivered >= limits.maxMessages)
	}
	for _, area := range []string{spool.HoldDir, spool.QueueDir} {
		ids, err := q.sp.List(area)
		if err != nil {
			return err
		}
		for _, id := range ids {
			if isOver() {
				q.log.Info("queue run budget spent")
				return nil
			}
			rec := q.lock(area, id, limits.owner)
			if rec == nil {
				continue
			}
			if area == spool.HoldDir && !q.release(rec) {
				_ = rec.Close()
				continue
			}
			if q.deliverEntry(ctx, rec) {
				delivered++
			}
		}
	}
	return nil
}

// lock returns the entry id of area locked, or nil when it belongs to
// another owner, is busy or gone, or cannot be read.
func (q *queue) lock(area, id string, owner int) *spool.Record {
	if owner >= 0 {
		// OwnerUID never changes, so reading it without the lock is safe.
		e, err := q.sp.Peek(area, id)
		if err != nil || e.OwnerUID != owner {
			return nil
		}
	}
	rec, err := q.sp.Lock(area, id)
	switch {
	case errors.Is(err, spool.ErrBusy), errors.Is(err, spool.ErrGone):
		return nil
	case errors.Is(err, spool.ErrUnknownVersion):
		q.log.Warn("spool entry of another version left alone", "id", id, "err", err)
		return nil
	case err != nil:
		q.log.Warn("spool entry unreadable, left alone", "id", id, "err", err)
		return nil
	}
	if q.d.entryLocked != nil {
		q.d.entryLocked(id)
	}
	return rec
}

// release moves a held entry to the queue with every configured target,
// or to failed/ when it expired; it reports whether the entry is now in
// the queue.
func (q *queue) release(rec *spool.Record) bool {
	if spool.Expired(rec.Entry, q.d.Now(), q.settings.HoldTTL.Duration) {
		q.fail(rec, reasonExpired)
		return false
	}
	if q.targets == nil {
		return false
	}
	q.setReleased(rec.Entry)
	if err := rec.Move(spool.QueueDir); err != nil {
		q.hasIOError = true
		q.log.Error("held message not released", "id", rec.ID(), "err", err)
		return false
	}
	q.log.Info("held message released", "id", rec.ID())
	return true
}

// setReleased gives a held entry every configured target and starts its
// time in the queue.
func (q *queue) setReleased(e *spool.Entry) {
	names := make([]string, 0, len(q.targets))
	for name := range q.targets {
		names = append(names, name)
	}
	slices.Sort(names)
	e.SetTargets(names)
	e.Reason, e.ReleasedAt = "", q.d.Now()
}

// releaseOtherHold moves the entries of hold/ in q.otherHold into this
// spool, of owner only unless owner is -1: into the queue, or into failed/
// when they expired, where mailq shows them and failed_ttl applies. The
// spools may be on different file systems, so the entry is written here
// first and removed there after: a crash in between repeats the delivery
// rather than losing it. It returns the other spool, nil when it cannot
// be opened.
func (q *queue) releaseOtherHold(owner int) *spool.Spool {
	other, err := spool.OpenExisting(q.otherHold)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			q.log.Warn("default spool not opened", "err", err)
		}
		return nil
	}
	ids, err := other.List(spool.HoldDir)
	if err != nil {
		q.log.Warn("default spool not listed", "err", err)
		return other
	}
	for _, id := range ids {
		if owner >= 0 {
			if e, err := other.Peek(spool.HoldDir, id); err != nil || e.OwnerUID != owner {
				continue
			}
		}
		rec, err := other.Lock(spool.HoldDir, id)
		if err != nil {
			continue
		}
		q.releaseInto(rec)
		_ = rec.Close()
	}
	return other
}

// releaseInto copies a locked held entry of another spool into this one
// and removes it there. An entry of the same id here is the copy of a run
// that died before the removal: it is kept as it is, since a process may
// be delivering it, and only the held one is removed.
func (q *queue) releaseInto(rec *spool.Record) {
	id := rec.ID()
	if q.sp.Has(spool.QueueDir, id) || q.sp.Has(spool.FailedDir, id) {
		q.removeReleased(rec, "held message already released")
		return
	}
	raw, err := rec.Message()
	if err != nil {
		q.log.Error("held message unreadable", "id", id, "err", err)
		return
	}
	entry, area, quota := *rec.Entry, spool.QueueDir, q.quota(rec.Entry.OwnerUID)
	isExpired := spool.Expired(rec.Entry, q.d.Now(), q.settings.HoldTTL.Duration)
	if isExpired {
		// failed/ counts in no limit.
		entry.Reason, entry.FailedAt = reasonExpired, q.d.Now()
		area, quota = spool.FailedDir, spool.Quota{}
	} else {
		q.setReleased(&entry)
	}
	copied, err := q.sp.Create(area, &entry, raw, quota)
	if err != nil {
		q.log.Error("held message not released", "id", id, "err", err)
		return
	}
	_ = copied.Close()
	if isExpired {
		q.log.Error("message failed", "id", id, "reason", reasonExpired)
		q.removeReleased(rec, "")
		return
	}
	q.removeReleased(rec, "held message released")
}

// removeReleased removes a held entry of another spool after its copy
// here, and logs message when not empty.
func (q *queue) removeReleased(rec *spool.Record, message string) {
	if err := rec.Remove(); err != nil {
		q.log.Error("held message released but not removed", "id", rec.ID(), "err", err)
		return
	}
	if message != "" {
		q.log.Info(message, "id", rec.ID(), "from", q.otherHold)
	}
}

// deliverEntry sends a locked queue entry to its targets that are due,
// records the results, and releases it. It reports whether it sent
// anything.
func (q *queue) deliverEntry(ctx context.Context, rec *spool.Record) bool {
	defer q.finish(rec)
	e, now := rec.Entry, q.d.Now()
	log := q.log.With("id", e.ID)
	if spool.Expired(e, now, q.settings.QueueTTL.Duration) {
		q.fail(rec, reasonExpired)
		return false
	}
	if q.targets == nil {
		return false
	}
	var due []delivery.Target
	isChanged := false
	for _, name := range sortedTargets(e) {
		state := e.Targets[name]
		target, ok := q.targets[name]
		switch {
		case state.State != spool.Pending:
		case !ok:
			e.MarkFailed(name, delivery.Perm.String(), reasonTargetRemoved)
			isChanged = true
			log.Error("target removed, message dropped for it", "target", name)
		case state.NextAt.After(now), q.throttled[name].After(now):
		default:
			due = append(due, target)
		}
	}
	if len(due) == 0 {
		if isChanged {
			q.save(rec)
		}
		return false
	}
	raw, err := rec.Message()
	if err != nil {
		q.hasIOError = true
		log.Error("queued message unreadable", "err", err)
		return false
	}
	msg, bcc, _, err := message.Read(bytes.NewReader(raw), message.ReadOptions{IgnoreDots: true, MaxSize: message.MaxSize, ReceivedAt: e.ReceivedAt})
	if err != nil {
		log.Error("queued message unreadable", "err", err)
		return false
	}
	data := render.NewData(msg, e.Envelope, bcc)
	data.Hostname, data.ReceivedAt, data.Strings = q.d.Hostname, e.ReceivedAt, q.notices
	ctx, cancel := context.WithTimeout(ctx, q.deadline)
	defer cancel()
	results := q.send(ctx, due, e.Envelope, data, msg.Attachments, msg.Raw, rec)
	for _, r := range results {
		logResult(log, r)
	}
	if !e.IsPending() {
		log.Info("queued message finished")
	}
	return true
}

// sortedTargets returns the target names of e in order, so that logs do
// not depend on map iteration.
func sortedTargets(e *spool.Entry) []string {
	names := make([]string, 0, len(e.Targets))
	for name := range e.Targets {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func (q *queue) save(rec *spool.Record) {
	if err := rec.Save(); err != nil {
		q.hasIOError = true
		q.log.Error("spool entry not updated", "id", rec.ID(), "err", err)
	}
}

// fail moves an entry to failed/ with reason.
func (q *queue) fail(rec *spool.Record, reason string) {
	rec.Entry.Reason, rec.Entry.FailedAt = reason, q.d.Now()
	if err := rec.Move(spool.FailedDir); err != nil {
		q.hasIOError = true
		q.log.Error("spool entry not moved to failed", "id", rec.ID(), "err", err)
		return
	}
	q.log.Error("message failed", "id", rec.ID(), "reason", reason)
}

// clean deletes the files that dead processes left in sp and entries
// that stayed in its failed/ longer than failed_ttl.
func (q *queue) clean(sp *spool.Spool) {
	now := q.d.Now()
	if removed, err := sp.RemoveStale(now, staleAge); err != nil {
		q.hasIOError = true
		q.log.Error("stale spool files not removed", "err", err)
	} else if removed > 0 {
		q.log.Info("stale spool files removed", "count", removed)
	}
	ids, err := sp.List(spool.FailedDir)
	if err != nil {
		q.hasIOError = true
		q.log.Error("failed messages not listed", "err", err)
		return
	}
	for _, id := range ids {
		rec, err := sp.Lock(spool.FailedDir, id)
		if err != nil {
			continue
		}
		if now.Sub(rec.Entry.FailedAt) > q.settings.FailedTTL.Duration {
			if err := rec.Remove(); err != nil {
				q.hasIOError = true
				q.log.Error("failed message not deleted", "id", id, "err", err)
			} else {
				q.log.Info("failed message deleted", "id", id)
			}
		}
		_ = rec.Close()
	}
}

// runQueue is -q: a run over the whole queue for root, the service user
// and a caller without elevation, over the own entries for everyone else.
// A run of the same user already under way makes it succeed at once.
func (q *queue) runQueue(ctx context.Context) int {
	if q.sp == nil {
		return exitOK
	}
	limits := runLimits{owner: -1, budget: q.settings.RunBudget.Duration}
	if !q.d.Credentials.isPrivilegedCaller() {
		limits.owner = q.d.Credentials.UID
		lock, err := q.sp.LockRun(q.d.Credentials.UID)
		if errors.Is(err, spool.ErrBusy) {
			q.log.Info("queue run of this user under way")
			return exitOK
		}
		if err != nil {
			q.log.Error("queue run lock not taken", "err", err)
			return exitIOErr
		}
		defer func() { _ = lock.Close() }()
	}
	var other *spool.Spool
	if q.otherHold != "" && q.targets != nil {
		other = q.releaseOtherHold(limits.owner)
	}
	if err := q.run(ctx, limits); err != nil {
		q.log.Error("queue not listed", "err", err)
		return exitIOErr
	}
	if limits.owner < 0 {
		q.clean(q.sp)
		if other != nil {
			q.clean(other)
		}
	}
	if q.hasIOError {
		return exitIOErr
	}
	return exitOK
}

// drainOwn is the short queue run after the own message of a call: the
// caller's entries only, under the caller's run lock, which a concurrent
// call of the same user keeps; errors are logged and do not change the
// exit status of the call.
func (q *queue) drainOwn(ctx context.Context) {
	if q.sp == nil {
		return
	}
	lock, err := q.sp.LockRun(q.d.Credentials.UID)
	if err != nil {
		return
	}
	defer func() { _ = lock.Close() }()
	limits := runLimits{owner: q.d.Credentials.UID, budget: q.settings.DrainBudget.Duration, maxMessages: q.settings.DrainMaxMessages}
	if err := q.run(ctx, limits); err != nil {
		q.log.Error("queue not listed", "err", err)
	}
}

// queueCounts summarize the spool for mailq and --status.
type queueCounts struct {
	queued, held, failed, tmp int
	bytes                     int64
	oldest                    time.Duration
}

// listQueue prints the spool: every entry with the state of its targets
// for a privileged caller, the counts alone for anyone else, who must not
// see the entries and errors of other users.
func (q *queue) listQueue(w io.Writer) int {
	if q.sp == nil {
		_, _ = fmt.Fprintln(w, "queue is empty")
		return exitOK
	}
	counts, lines, err := q.scan(q.d.Credentials.isPrivilegedCaller())
	if err != nil {
		q.log.Error("queue not listed", "err", err)
		return exitIOErr
	}
	if counts.queued+counts.held+counts.failed == 0 {
		_, _ = fmt.Fprintln(w, "queue is empty")
		return exitOK
	}
	_, _ = fmt.Fprintf(w, "%d queued, %d held, %d failed; oldest %s\n", counts.queued, counts.held, counts.failed, counts.oldest)
	for _, line := range lines {
		_, _ = fmt.Fprintln(w, line)
	}
	return exitOK
}

// status prints the counts as one logfmt line for monitoring.
func (q *queue) status(w io.Writer) int {
	var counts queueCounts
	if q.sp != nil {
		var err error
		if counts, _, err = q.scan(false); err != nil {
			q.log.Error("queue not listed", "err", err)
			return exitIOErr
		}
	}
	_, _ = fmt.Fprintf(w, "queued=%d held=%d failed=%d tmp=%d bytes=%d oldest_age_seconds=%d\n",
		counts.queued, counts.held, counts.failed, counts.tmp, counts.bytes, int64(counts.oldest/time.Second))
	return exitOK
}

// scan counts the entries of the spool and, with isDetailed, describes
// each one.
func (q *queue) scan(isDetailed bool) (queueCounts, []string, error) {
	var counts queueCounts
	var lines []string
	now := q.d.Now()
	usage, err := q.sp.Usage()
	if err != nil {
		return counts, nil, err
	}
	counts.bytes = usage.Bytes
	for _, area := range []string{spool.QueueDir, spool.HoldDir, spool.FailedDir, spool.TmpDir} {
		ids, err := q.sp.List(area)
		if err != nil {
			return counts, nil, err
		}
		switch area {
		case spool.QueueDir:
			counts.queued = len(ids)
		case spool.HoldDir:
			counts.held = len(ids)
		case spool.FailedDir:
			counts.failed = len(ids)
		case spool.TmpDir:
			counts.tmp = len(ids)
			continue
		}
		for _, id := range ids {
			e, err := q.sp.Peek(area, id)
			if err != nil {
				if isDetailed {
					lines = append(lines, fmt.Sprintf("%s %s unreadable", id, area))
				}
				continue
			}
			age := now.Sub(e.CreatedAt).Truncate(time.Second)
			if area != spool.FailedDir && age > counts.oldest {
				counts.oldest = age
			}
			if isDetailed {
				lines = append(lines, describeEntry(area, e, age)...)
			}
		}
	}
	return counts, lines, nil
}

// describeEntry returns the mailq lines of one entry: the entry, then one
// indented line per target.
func describeEntry(area string, e *spool.Entry, age time.Duration) []string {
	head := fmt.Sprintf("%s %s uid=%d age=%s", e.ID, area, e.OwnerUID, age)
	if e.Reason != "" {
		head += fmt.Sprintf(" reason=%q", e.Reason)
	}
	lines := []string{head}
	for _, name := range sortedTargets(e) {
		state := e.Targets[name]
		line := fmt.Sprintf("  %s %s attempts=%d", name, state.State, state.Attempts)
		if state.State == spool.Pending && !state.NextAt.IsZero() {
			line += " next=" + state.NextAt.UTC().Format(time.RFC3339)
		}
		if state.LastError != "" {
			line += fmt.Sprintf(" %s=%q", state.LastClass, strings.ToValidUTF8(state.LastError, ""))
		}
		lines = append(lines, line)
	}
	return lines
}
