package app

import (
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

	"github.com/6RUN0/mailcrier/internal/backend"
	"github.com/6RUN0/mailcrier/internal/backend/hook"
	"github.com/6RUN0/mailcrier/internal/config"
	"github.com/6RUN0/mailcrier/internal/delivery"
	"github.com/6RUN0/mailcrier/internal/message"
	"github.com/6RUN0/mailcrier/internal/redact"
	"github.com/6RUN0/mailcrier/internal/render"
	"github.com/6RUN0/mailcrier/internal/route"
	"github.com/6RUN0/mailcrier/internal/spool"
)

// staleAge is the age after which a file in tmp/ is the rest of a process
// that died while writing it: writing an entry takes milliseconds.
const staleAge = time.Hour

// Reasons stored in the sidecar and logged when an entry leaves the queue
// without delivery.
const (
	reasonConfig        = "configuration rejected"
	reasonNoRoute       = "no route"
	reasonExpired       = "expired"
	reasonTargetRemoved = "target removed"
	// reasonCorrupt moves an entry whose sidecar does not decode to
	// failed/, once its message file is older than the TTL of its area.
	reasonCorrupt = "corrupt sidecar"
	// reasonInternal moves an entry to failed/ once no target waits for
	// it, when a target failed by a panic: a retry would repeat the bug,
	// and the message stays where mailq shows it.
	reasonInternal = "internal error"
)

// classInternal is the class stored for a target that failed by a panic,
// beside the "temp" and "perm" of delivery.Status.
const classInternal = "internal"

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
	// router applies the rules of the configuration; nil when it was
	// rejected.
	router *route.Router
	// direct makes the targets of direct chats; nil without
	// telegram_direct.
	direct *directChats
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
	// hasPanicked records a panic on one entry of a queue run, which
	// makes -q exit 70.
	hasPanicked bool
	// unsaved holds the ids of the entries whose new state was not
	// written: on disk they look due at once, and a run of this process
	// skips them rather than send them again.
	unsaved map[string]bool
	// otherHold is another spool directory whose hold/ a queue run
	// releases into this queue; empty for none.
	otherHold string
	// isOwnTaken reports that deliverOwn tried the spool for the own
	// message: it is in queue/ or goes out without an entry, and a panic
	// after that must not hold it once more.
	isOwnTaken bool
}

// newQueue returns the queue of the spool settings with targets, which may
// be nil. When the directory cannot be opened the queue works without the
// spool, and openErr tells the caller why.
func newQueue(d Deps, log *slog.Logger, redactor *redact.Redactor, settings config.Spool, targets []delivery.Target) (q *queue, openErr error) {
	q = &queue{d: d, log: log, redactor: redactor, settings: settings, throttled: map[string]time.Time{}, unsaved: map[string]bool{}}
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
		state = queueState(spoolErr)
	}
	// The id leads from every record of the message to its line in mailq.
	log := q.log
	if rec != nil {
		log = log.With("id", rec.ID())
		log.Debug("message queued ahead of delivery")
	}
	q.isOwnTaken = true
	results, donePanic := q.send(hook.WithLog(ctx, log), targets, env, data, msg.Attachments, msg.Raw, rec)
	for _, r := range results {
		logResult(log, q.redactor, r, state == delivery.Queued)
	}
	if rec != nil {
		q.finish(rec)
	}
	if spoolErr != nil {
		// Without a temporary failure nothing needed the entry. The error
		// tells a directory not opened from an entry not created or not
		// written.
		level := slog.LevelWarn
		if slices.ContainsFunc(results, func(r delivery.Result) bool { return r.Status == delivery.Temp }) {
			level = slog.LevelError
		}
		q.log.Log(ctx, level, "message not spooled", "err", spoolErr)
	}
	logOutcome(log, results, state)
	if donePanic != nil {
		// Recorded and logged, the results are safe; the call ends as on
		// any panic, without its queue run.
		panic(donePanic)
	}
	return delivery.ExitCode(results, state)
}

// send delivers to targets and records each result in rec, when not nil,
// as soon as the target is finished. raw is the kept input of the
// message, for a target that sends it as a file. donePanic is a panic of
// the recording in the goroutine of a target: the results it left
// unrecorded are recorded again here, where a repeated panic reaches the
// recover of the caller.
func (q *queue) send(ctx context.Context, targets []delivery.Target, env message.Envelope, data render.Data, files []message.Attachment, raw []byte, rec *spool.Record) (results []delivery.Result, donePanic *backend.PanicError) {
	// recorded holds the targets whose result reached the entry, applied
	// and written or its write failure logged.
	recorded := map[string]bool{}
	record := func(r delivery.Result) {
		q.mu.Lock()
		defer q.mu.Unlock()
		q.throttle(r)
		if rec == nil {
			return
		}
		q.apply(rec.Entry, r)
		// The entry of the last result is removed, or moved to failed/,
		// at once: a Save just before would cost a sync. On failure Save
		// records the result, and finish tries again.
		if !rec.Entry.IsPending() && q.retire(rec) {
			recorded[r.TargetID] = true
			if q.d.spoolSaved != nil {
				q.d.spoolSaved(rec.Entry)
			}
			return
		}
		err := rec.Save()
		recorded[r.TargetID] = true
		if err != nil {
			q.hasIOError = true
			q.unsaved[rec.ID()] = true
			q.log.Error("spool entry not updated", "id", rec.ID(), "target", r.TargetID, "err", err)
			return
		}
		if q.d.spoolSaved != nil {
			q.d.spoolSaved(rec.Entry)
		}
	}
	if q.d.deliver != nil {
		results = q.d.deliver(ctx, targets, env, data, files)
		for _, r := range results {
			record(r)
		}
		return results, nil
	}
	results, donePanic = delivery.DeliverEach(ctx, targets, data, files, raw, record)
	for _, r := range results {
		if donePanic != nil && rec != nil && !recorded[r.TargetID] {
			record(r)
		}
	}
	return results, donePanic
}

// apply records the result of one target in e. The error is stored
// without its class, which mailq shows as the key: "status 503: Service
// Unavailable".
func (q *queue) apply(e *spool.Entry, r delivery.Result) {
	errText := ""
	if r.Err != nil {
		errText = failureCause(r.Err).Error()
		if deliveryErr, ok := r.Err.(*backend.Error); ok && deliveryErr.Status != 0 {
			errText = fmt.Sprintf("status %d: %s", deliveryErr.Status, errText)
		}
		errText = q.redactor.String(errText)
	}
	switch {
	case r.Status == delivery.OK:
		e.MarkDone(r.TargetID)
	case r.Status == delivery.Temp:
		e.MarkRetry(r.TargetID, q.d.Now(), retryAfter(r.Err), delivery.Temp.String(), errText)
	case r.IsInternal():
		e.MarkFailed(r.TargetID, classInternal, errText)
	default:
		e.MarkFailed(r.TargetID, delivery.Perm.String(), errText)
	}
}

// hasInternalFailure reports whether a target of e failed by a panic.
func hasInternalFailure(e *spool.Entry) bool {
	for _, target := range e.Targets {
		if target.State == spool.Failed && target.LastClass == classInternal {
			return true
		}
	}
	return false
}

// retire disposes of an entry that no target waits for: it moves one with
// a target that failed by a panic to failed/ and removes any other. It
// reports whether that succeeded; a failed removal is left to finish.
func (q *queue) retire(rec *spool.Record) bool {
	switch {
	case rec.Area == spool.FailedDir:
		return true
	case hasInternalFailure(rec.Entry):
		return q.fail(rec, reasonInternal)
	default:
		return rec.Remove() == nil
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

// finish disposes of an entry that no target waits for, as retire does,
// and releases it.
func (q *queue) finish(rec *spool.Record) {
	switch {
	case rec.Entry.IsPending(), rec.Area == spool.FailedDir:
	case hasInternalFailure(rec.Entry):
		q.fail(rec, reasonInternal)
	default:
		if err := rec.Remove(); err != nil {
			q.hasIOError = true
			q.log.Error("spool entry not removed", "id", rec.ID(), "err", err)
		}
	}
	_ = rec.Close()
}

// hold puts a message into hold/ for reason: its configuration was
// rejected, or its rules select no target. A queue run takes it once the
// configuration loads again and routes it. It returns Queued when the
// message is held, QueueOff with the spool off, and otherwise why the
// entry is missing.
func (q *queue) hold(msg *message.Message, env message.Envelope, receivedAt time.Time, openErr error, reason string) delivery.Queue {
	if q.settings.Dir == "" {
		q.log.Error("message lost, spool off", "reason", reason)
		return delivery.QueueOff
	}
	err := openErr
	if err == nil {
		entry := spool.NewEntry(spool.NewID(q.d.Now()), q.d.Credentials.UID, q.d.Now(), receivedAt, env, nil)
		entry.Reason = reason
		var rec *spool.Record
		if rec, err = q.sp.Create(spool.HoldDir, entry, msg.Raw, q.quota(entry.OwnerUID)); err == nil {
			q.log.Warn("message held", "id", rec.ID(), "reason", reason)
			_ = rec.Close()
			return delivery.Queued
		}
	}
	q.log.Error("message lost, not held", "reason", reason, "err", err)
	return queueState(err)
}

// queueState returns the state of a spool entry for which Create, or Open
// before it, returned err.
func queueState(err error) delivery.Queue {
	switch {
	case err == nil:
		return delivery.Queued
	case errors.Is(err, spool.ErrWrite):
		return delivery.QueueNotWritten
	default:
		return delivery.QueueNotCreated
	}
}

// runLimits bound one queue run.
type runLimits struct {
	// owner restricts the run to the entries of one uid; -1 takes all.
	owner int
	// budget bounds the run, budgetKey names its key for the error of a
	// target that runs out of it; maxMessages bounds the entries it
	// delivers, 0 for no bound.
	budget      time.Duration
	budgetKey   string
	maxMessages int
}

// run works through hold/ and queue/, oldest entries first, and returns
// an error when the spool cannot be listed. Entries locked by another
// process are skipped: that process delivers them. A panic on one entry
// fails that entry alone, see recoverEntry.
func (q *queue) run(ctx context.Context, limits runLimits) error {
	ctx, cancel := context.WithTimeoutCause(ctx, limits.budget, &backend.LimitError{Key: limits.budgetKey, Value: limits.budget})
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
				q.logStop(ctx, delivered, limits)
				return nil
			}
			if q.isUnsaved(id) {
				q.log.Debug("spool entry skipped, state not recorded", "id", id)
				continue
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

// logStop records why a queue run stopped before its end: a stop signal,
// or its budget of time or messages, with the count it delivered.
func (q *queue) logStop(ctx context.Context, delivered int, limits runLimits) {
	var limit *backend.LimitError
	if cause := context.Cause(ctx); cause != nil && !errors.As(cause, &limit) {
		q.log.Info("queue run stopped", "delivered", delivered, "err", cause)
		return
	}
	q.log.Info("queue run budget spent", "delivered", delivered, "budget", limits.budget)
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
	if err != nil {
		q.lockFailed(q.sp, area, id, err)
		return nil
	}
	if q.d.entryLocked != nil {
		q.d.entryLocked(id)
	}
	return rec
}

// lockFailed handles the error of Lock on the entry id of area in sp. An
// entry of another version is left alone, for the release that wrote it.
// One whose sidecar is corrupt moves to failed/ once its message file is
// older than the TTL of its area, see failCorrupt; until then, like an
// entry that cannot be read at all, it makes -q exit 74.
func (q *queue) lockFailed(sp *spool.Spool, area, id string, err error) {
	switch {
	case errors.Is(err, spool.ErrOrphanRemoved):
		q.log.Info("message without sidecar removed", "id", id, "area", area)
	case errors.Is(err, spool.ErrBusy), errors.Is(err, spool.ErrGone):
	case errors.Is(err, spool.ErrUnknownVersion):
		q.log.Warn("spool entry of another version left alone", "id", id, "area", area, "err", err)
	case errors.Is(err, spool.ErrCorrupt) && q.failCorrupt(sp, area, id):
	default:
		q.hasIOError = true
		q.log.Warn("spool entry unreadable, left alone", "id", id, "area", area, "err", err)
	}
}

// failCorrupt moves the entry id of area in sp, whose sidecar is corrupt,
// to failed/ with reasonCorrupt once its message file is older than
// queue_ttl, or hold_ttl in hold/. It reports false when the entry stays
// where it is, unexpired or unreadable for another reason; fail logs a
// failed move. The sidecar that holds the dates is unreadable, and the
// message file is written once, when the entry is.
func (q *queue) failCorrupt(sp *spool.Spool, area, id string) (isHandled bool) {
	rec, err := sp.LockCorrupt(area, id)
	switch {
	case errors.Is(err, spool.ErrBusy), errors.Is(err, spool.ErrGone):
		return true
	case err != nil:
		return false
	}
	defer func() { _ = rec.Close() }()
	// A panic from here on is handled: recoverEntry fails the entry.
	isHandled = true
	defer q.recoverEntry(sp, rec)
	ttl := q.settings.QueueTTL.Duration
	if area == spool.HoldDir {
		ttl = q.settings.HoldTTL.Duration
	}
	if !spool.Expired(rec.Entry, q.d.Now(), ttl) {
		return false
	}
	q.fail(rec, reasonCorrupt)
	return true
}

// release moves a held entry to the queue with the targets its rules
// select, or to failed/ when it expired; it removes a suppressed one and
// keeps one without a route. It reports whether the entry is now in the
// queue.
func (q *queue) release(rec *spool.Record) bool {
	defer q.recoverEntry(q.sp, rec)
	if spool.Expired(rec.Entry, q.d.Now(), q.settings.HoldTTL.Duration) {
		q.fail(rec, reasonExpired)
		return false
	}
	if q.targets == nil {
		return false
	}
	log := q.log.With("id", rec.ID())
	names, v, err := q.routeHeld(log, rec.Entry, rec.Message)
	switch {
	case err != nil:
		// A spool that cannot be read makes -q exit 74; routing parses
		// only the subject, which does not fail.
		q.hasIOError = true
		log.Error("held message unreadable", "err", err)
		return false
	case v == suppressed:
		if err := rec.Remove(); err != nil {
			q.hasIOError = true
			log.Error("spool entry not removed", "err", err)
		}
		return false
	case v == noRoute:
		if q.markNoRoute(log, rec.Entry) {
			q.save(rec)
		}
		return false
	}
	q.setReleased(rec.Entry, names)
	if err := rec.Move(spool.QueueDir); err != nil {
		q.hasIOError = true
		q.log.Error("held message not released", "id", rec.ID(), "err", err)
		return false
	}
	q.log.Info("held message released", "id", rec.ID())
	return true
}

// setReleased gives a held entry the targets of names and starts its time
// in the queue.
func (q *queue) setReleased(e *spool.Entry, names []string) {
	e.SetTargets(names)
	e.Reason, e.ReleasedAt = "", q.d.Now()
}

// markNoRoute records in a held entry that its rules select no target and
// reports whether that changed the entry. The warning comes once per
// change of reason; a queue run that finds it again logs at debug.
func (q *queue) markNoRoute(log *slog.Logger, e *spool.Entry) bool {
	isChanged := e.Reason != reasonNoRoute
	log.Log(context.Background(), repeatLevel(!isChanged), "no route for message")
	e.Reason = reasonNoRoute
	return isChanged
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
			q.lockFailed(other, spool.HoldDir, id, err)
			continue
		}
		func() {
			defer q.recoverEntry(other, rec)
			q.releaseInto(rec)
		}()
		_ = rec.Close()
	}
	return other
}

// releaseInto copies a locked held entry of another spool into this one
// and removes it there: into the queue with the targets its rules select,
// into hold/ when they select none, into failed/ when it expired. A
// suppressed entry is only removed. An entry of the same id here is the
// copy of a run that died before the removal: it is kept as it is, since a
// process may be delivering it, and only the held one is removed.
func (q *queue) releaseInto(rec *spool.Record) {
	id := rec.ID()
	if q.sp.Has(spool.QueueDir, id) || q.sp.Has(spool.HoldDir, id) || q.sp.Has(spool.FailedDir, id) {
		q.removeReleased(rec, "held message already released")
		return
	}
	raw, err := rec.Message()
	if err != nil {
		q.hasIOError = true
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
		log := q.log.With("id", id)
		names, v, err := q.routeHeld(log, &entry, func() ([]byte, error) { return raw, nil })
		switch {
		case err != nil:
			q.hasIOError = true
			log.Error("held message unreadable", "err", err)
			return
		case v == suppressed:
			q.removeReleased(rec, "")
			return
		case v == noRoute:
			q.markNoRoute(log, &entry)
			area = spool.HoldDir
		default:
			q.setReleased(&entry, names)
		}
	}
	copied, err := q.sp.Create(area, &entry, raw, quota)
	if err != nil {
		q.log.Error("held message not released", "id", id, "err", err)
		return
	}
	_ = copied.Close()
	switch {
	case isExpired:
		q.log.Error("message failed", "id", id, "area", spool.HoldDir, "reason", reasonExpired, "dir", q.otherHold)
		q.removeReleased(rec, "")
	case area == spool.HoldDir:
		q.removeReleased(rec, "held message moved")
	default:
		q.removeReleased(rec, "held message released")
	}
}

// removeReleased removes a held entry of another spool after its copy
// here, and logs message when not empty.
func (q *queue) removeReleased(rec *spool.Record, message string) {
	if err := rec.Remove(); err != nil {
		q.log.Error("held message released but not removed", "id", rec.ID(), "err", err)
		return
	}
	if message != "" {
		q.log.Info(message, "id", rec.ID(), "dir", q.otherHold)
	}
}

// deliverEntry sends a locked queue entry to its targets that are due,
// records the results, and releases it. It reports whether it sent
// anything.
func (q *queue) deliverEntry(ctx context.Context, rec *spool.Record) bool {
	defer q.finish(rec)
	defer q.recoverEntry(q.sp, rec)
	e, now := rec.Entry, q.d.Now()
	log := q.log.With("id", e.ID)
	if spool.Expired(e, now, q.settings.QueueTTL.Duration) {
		// A target that failed by a panic names the cause better than the
		// age of the entry.
		reason := reasonExpired
		if hasInternalFailure(e) {
			reason = reasonInternal
		}
		q.fail(rec, reason)
		return false
	}
	if q.targets == nil {
		return false
	}
	var due []delivery.Target
	isChanged := false
	for _, name := range sortedTargets(e) {
		state := e.Targets[name]
		target, ok := q.target(name)
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
	msg, bcc, _, err := readStored(raw, e)
	if err != nil {
		q.hasIOError = true
		log.Error("queued message unreadable", "err", err)
		return false
	}
	data := render.NewData(msg, e.Envelope, bcc)
	data.Hostname, data.ReceivedAt, data.Strings = q.d.Hostname, e.ReceivedAt, q.notices
	ctx, cancel := withDeadline(hook.WithLog(ctx, log), q.deadline)
	defer cancel()
	results, donePanic := q.send(ctx, due, e.Envelope, data, msg.Attachments, msg.Raw, rec)
	for _, r := range results {
		logResult(log, q.redactor, r, true)
	}
	if !e.IsPending() {
		log.Info("queued message finished")
	}
	if donePanic != nil {
		// send recorded every result, so the entry keeps its pending
		// targets; only a repeated panic fails it, see recoverEntry.
		q.notePanic(log, donePanic)
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
		q.mu.Lock()
		defer q.mu.Unlock()
		q.hasIOError = true
		q.unsaved[rec.ID()] = true
		q.log.Error("spool entry not updated", "id", rec.ID(), "err", err)
	}
}

// isUnsaved reports whether the new state of the entry id was not written
// in this process.
func (q *queue) isUnsaved(id string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.unsaved[id]
}

// recoverEntry is deferred by the steps of a queue run on one locked
// entry of sp. A panic there, a bug, is logged with its stack and moves the
// entry to failed/ of sp with reasonInternal, so that the next run does
// not die on it again, and the run goes on with the next entry.
func (q *queue) recoverEntry(sp *spool.Spool, rec *spool.Record) {
	value := recover()
	if value == nil {
		return
	}
	q.notePanic(q.log.With("id", rec.ID()), value)
	if rec.Area != spool.FailedDir && sp.Has(rec.Area, rec.ID()) {
		q.fail(rec, reasonInternal)
	}
}

// notePanic logs a panic on one entry of a queue run, which makes -q
// exit 70.
func (q *queue) notePanic(log *slog.Logger, value any) {
	q.hasPanicked = true
	log.Error("panic in queue run", panicAttrs(q.redactor, value)...)
}

// fail moves an entry to failed/ with reason and reports whether it did.
// The record names the area the entry left and the targets that still
// waited for it.
func (q *queue) fail(rec *spool.Record, reason string) bool {
	attrs := []any{"id", rec.ID(), "area", rec.Area, "reason", reason}
	if pending := pendingTargets(rec.Entry); len(pending) > 0 {
		attrs = append(attrs, "targets", pending)
	}
	rec.Entry.Reason, rec.Entry.FailedAt = reason, q.d.Now()
	if err := rec.Move(spool.FailedDir); err != nil {
		q.hasIOError = true
		if rec.Area != spool.FailedDir {
			q.log.Error("spool entry not moved to failed", "id", rec.ID(), "err", err)
			return false
		}
		// Both files are in failed/; syncing a directory or removing the
		// old sidecar, which RemoveStale deletes later, failed.
		q.log.Warn("spool entry moved to failed, not synced", "id", rec.ID(), "err", err)
	}
	q.log.Error("message failed", attrs...)
	return true
}

// pendingTargets returns the names of the targets of e that wait for a
// delivery, in order.
func pendingTargets(e *spool.Entry) []string {
	var names []string
	for _, name := range sortedTargets(e) {
		if e.Targets[name].State == spool.Pending {
			names = append(names, name)
		}
	}
	return names
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
		if errors.Is(err, spool.ErrCorrupt) {
			// Without FailedAt, failed_ttl counts from the time the entry
			// was written.
			if rec, err = sp.LockCorrupt(spool.FailedDir, id); err == nil {
				rec.Entry.FailedAt = rec.Entry.CreatedAt
			}
		}
		if errors.Is(err, spool.ErrOrphanRemoved) {
			q.log.Info("message without sidecar removed", "id", id, "area", spool.FailedDir)
		}
		if err != nil {
			continue
		}
		func() {
			defer q.recoverEntry(sp, rec)
			if now.Sub(rec.Entry.FailedAt) > q.settings.FailedTTL.Duration {
				if err := rec.Remove(); err != nil {
					q.hasIOError = true
					q.log.Error("failed message not deleted", "id", id, "err", err)
				} else {
					q.log.Info("failed message deleted", "id", id)
				}
			}
		}()
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
	limits := runLimits{owner: -1, budget: q.settings.RunBudget.Duration, budgetKey: "run_budget"}
	if !q.d.Credentials.isPrivilegedCaller() {
		limits.owner = q.d.Credentials.UID
		lock, err := q.sp.LockRun(q.d.Credentials.UID)
		if errors.Is(err, spool.ErrBusy) {
			q.log.Info("queue run of this user under way")
			return exitOK
		}
		if err != nil {
			q.log.Error("queue run lock not taken", "err", err)
			tellUser(q.d, q.redactor, "queue run lock not taken", err)
			return exitIOErr
		}
		defer func() { _ = lock.Close() }()
	}
	var other *spool.Spool
	if q.otherHold != "" && q.targets != nil {
		other = q.releaseOtherHold(limits.owner)
	}
	if err := q.run(ctx, limits); err != nil {
		q.log.Error("queue not listed", "mode", "run", "err", err)
		tellUser(q.d, q.redactor, "queue not listed", err)
		return exitIOErr
	}
	if limits.owner < 0 {
		q.clean(q.sp)
		if other != nil {
			q.clean(other)
		}
	}
	if q.hasPanicked || q.hasIOError {
		// The records of the entries are in the log, one per entry.
		tellUser(q.d, q.redactor, "queue run incomplete, see the mail log", nil)
	}
	if q.hasPanicked {
		return exitSoftware
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
		// A run under way is the normal case; any other error, such as
		// locks/ not being a writable directory, stops every drain.
		if !errors.Is(err, spool.ErrBusy) {
			q.log.Warn("queue run lock not taken", "err", err)
		}
		return
	}
	defer func() { _ = lock.Close() }()
	limits := runLimits{owner: q.d.Credentials.UID, budget: q.settings.DrainBudget.Duration, budgetKey: "drain_budget", maxMessages: q.settings.DrainMaxMessages}
	if err := q.run(ctx, limits); err != nil {
		q.log.Error("queue not listed", "mode", "drain", "err", err)
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
	empty := "queue is empty\n"
	if q.settings.Dir == "" {
		// Nothing is queued with the spool off, which an empty queue would
		// hide.
		empty = "spool off\n"
	}
	if q.sp == nil && q.otherHold == "" {
		return q.print(w, empty)
	}
	counts, lines, err := q.scan(q.d.Credentials.isPrivilegedCaller())
	if err != nil {
		q.log.Error("queue not listed", "mode", "mailq", "err", err)
		tellUser(q.d, q.redactor, "queue not listed", err)
		return exitIOErr
	}
	if counts.queued+counts.held+counts.failed == 0 {
		return q.print(w, empty)
	}
	var out strings.Builder
	_, _ = fmt.Fprintf(&out, "%d queued, %d held, %d failed; oldest %s\n", counts.queued, counts.held, counts.failed, counts.oldest)
	for _, line := range lines {
		out.WriteString(line + "\n")
	}
	return q.print(w, out.String())
}

// status prints the counts as one logfmt line for monitoring.
func (q *queue) status(w io.Writer) int {
	var counts queueCounts
	if q.sp != nil || q.otherHold != "" {
		var err error
		if counts, _, err = q.scan(false); err != nil {
			q.log.Error("queue not listed", "mode", "status", "err", err)
			tellUser(q.d, q.redactor, "queue not listed", err)
			return exitIOErr
		}
	}
	return q.print(w, fmt.Sprintf("queued=%d held=%d failed=%d tmp=%d bytes=%d oldest_age_seconds=%d\n",
		counts.queued, counts.held, counts.failed, counts.tmp, counts.bytes, int64(counts.oldest/time.Second)))
}

// print writes text to w in one call and returns 0, or 74 when that
// fails: a full disk under a redirection must not read as an empty queue.
func (q *queue) print(w io.Writer, text string) int {
	if _, err := io.WriteString(w, text); err != nil {
		q.log.Error("queue listing not written", "err", err)
		tellUser(q.d, q.redactor, "queue listing not written", err)
		return exitIOErr
	}
	return exitOK
}

// scan counts the entries of the spool and of the hold/ of otherHold and,
// with isDetailed, describes each one; the latter with the field dir.
// bytes and tmp are those of the spool alone. The spool may be nil: then
// only otherHold is scanned.
func (q *queue) scan(isDetailed bool) (queueCounts, []string, error) {
	var counts queueCounts
	var lines []string
	now := q.d.Now()
	add := func(sp *spool.Spool, area, dir string, ids []string) {
		for _, id := range ids {
			e, err := sp.Peek(area, id)
			if err != nil {
				// The cause, as a queue run logs it, tells what to do.
				if isDetailed {
					lines = append(lines, fmt.Sprintf("%s %s unreadable", id, area)+dirField(dir)+fmt.Sprintf(" err=%q", strings.ToValidUTF8(err.Error(), "")))
				}
				continue
			}
			age := now.Sub(e.CreatedAt).Truncate(time.Second)
			if area != spool.FailedDir && age > counts.oldest {
				counts.oldest = age
			}
			if isDetailed {
				entry := describeEntry(area, e, age)
				entry[0] += dirField(dir)
				lines = append(lines, entry...)
			}
		}
	}
	if q.sp != nil {
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
			add(q.sp, area, "", ids)
		}
	}
	if q.otherHold == "" {
		return counts, lines, nil
	}
	// As in releaseOtherHold, a default directory this caller cannot read
	// leaves the listing of the spool itself standing.
	other, err := spool.OpenExisting(q.otherHold)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			q.log.Warn("default spool not opened", "err", err)
		}
		return counts, lines, nil
	}
	ids, err := other.List(spool.HoldDir)
	if err != nil {
		q.log.Warn("default spool not listed", "err", err)
		return counts, lines, nil
	}
	counts.held += len(ids)
	add(other, spool.HoldDir, q.otherHold, ids)
	return counts, lines, nil
}

// dirField returns the field that names the directory of an entry outside
// the spool, empty for one of the spool.
func dirField(dir string) string {
	if dir == "" {
		return ""
	}
	return fmt.Sprintf(" dir=%q", dir)
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
