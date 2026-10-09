// Package app wires configuration, message, targets and delivery into one
// sendmail invocation. Everything the process takes from the system comes
// in through Deps, so a whole invocation runs in tests without /etc, a
// syslog daemon or a network.
package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/6RUN0/mailcrier/internal/backend"
	"github.com/6RUN0/mailcrier/internal/backend/discord"
	"github.com/6RUN0/mailcrier/internal/backend/hook"
	"github.com/6RUN0/mailcrier/internal/backend/ntfy"
	"github.com/6RUN0/mailcrier/internal/backend/shoutrrr"
	"github.com/6RUN0/mailcrier/internal/backend/slack"
	"github.com/6RUN0/mailcrier/internal/backend/telegram"
	"github.com/6RUN0/mailcrier/internal/backend/webhook"
	"github.com/6RUN0/mailcrier/internal/config"
	"github.com/6RUN0/mailcrier/internal/delivery"
	"github.com/6RUN0/mailcrier/internal/message"
	"github.com/6RUN0/mailcrier/internal/redact"
	"github.com/6RUN0/mailcrier/internal/render"
	"github.com/6RUN0/mailcrier/internal/sendmail"
	"github.com/6RUN0/mailcrier/internal/spool"
	"github.com/6RUN0/mailcrier/internal/text"
)

// Exit statuses from sysexits.h that Run produces itself.
const (
	exitOK          = 0
	exitUsage       = 64
	exitNoInput     = 66
	exitUnavailable = 69
	exitSoftware    = 70
	exitCantCreate  = 73
	exitIOErr       = 74
	exitTempFail    = 75
	exitNoPerm      = 77
	exitConfig      = 78
)

// maxStackLength bounds the stack in the record of a panic: rsyslog keeps
// 8 KiB of a message by default (maxMessageSize), the other fields included.
const maxStackLength = 4096

// stackText returns stack redacted and cut to maxStackLength, after the
// redaction: a secret cut in half would no longer match.
func stackText(redactor *redact.Redactor, stack []byte) string {
	text := redactor.String(string(stack))
	if len(text) > maxStackLength {
		text = strings.ToValidUTF8(text[:maxStackLength], "")
	}
	return text
}

// callRecovering calls f and reports whether it panicked; the panic ends
// there.
func callRecovering(f func()) (hasPanicked bool) {
	defer func() { hasPanicked = recover() != nil }()
	f()
	return false
}

// panicAttrs returns the fields of the record of a recovered panic, its
// value and its stack: those of the goroutine where it happened when
// delivery raised it again as a *backend.PanicError.
func panicAttrs(redactor *redact.Redactor, value any) []any {
	if panicErr, ok := value.(*backend.PanicError); ok {
		return []any{"panic", panicErr.Value, "stack", stackText(redactor, panicErr.Stack)}
	}
	return []any{"panic", value, "stack", stackText(redactor, debug.Stack())}
}

// envConfig names another configuration file, like --config.
const envConfig = "MAILCRIER_CONFIG"

// Deps are the parts of the environment an invocation uses.
type Deps struct {
	// NewLogger returns the logger for a syslog tag. Run calls it once with
	// the default tag and again when the configuration sets another one.
	NewLogger func(tag string) *slog.Logger
	// ConfigFS is the file system root, "/" in a real invocation; ConfigPath
	// is the default configuration file relative to it. An absolute path
	// from --config or MAILCRIER_CONFIG and the secret files named in the
	// configuration are read from ConfigFS with the leading slash removed.
	ConfigFS   fs.FS
	ConfigPath string
	// HTTP performs the requests of HTTP-based targets. Run uses a copy
	// with the configured request timeout.
	HTTP *http.Client
	// Hostname is the name of the machine, sent along with each message.
	Hostname string
	// Now returns the current time; it dates a message without a Date
	// header.
	Now func() time.Time
	// Program is argv[0]; its base name selects the newaliases and mailq
	// modes.
	Program string
	// Stdout receives the output of mailq, --version and --help.
	Stdout io.Writer
	// Stderr receives what the standard log package writes: net/http and
	// its HTTP/2 transport print their debug output there.
	Stderr io.Writer
	// SetLogOutput redirects the standard log package, as log.SetOutput.
	SetLogOutput func(w io.Writer)
	// Credentials decide whether the process runs with the group of a
	// setgid binary.
	Credentials Credentials
	// Environ is the environment of the process, as os.Environ, after
	// Harden.
	Environ []string
	// ReexecErr is the error Harden returned: an elevated process that
	// could not execute itself with a clean environment.
	ReexecErr error
	// LookupUserName returns the login name of a uid from the user
	// database; false when there is none.
	LookupUserName func(uid int) (string, bool)
	// IsLogOnStderr reports whether the log goes to stderr, without a
	// syslog socket; nil means it never does.
	IsLogOnStderr func() bool
	// SpoolDir is the spool directory when the configuration sets none,
	// and where a message goes when the configuration is rejected; empty
	// turns the spool off.
	SpoolDir string
	// CatchSignals returns a context that a stop signal cancels, and the
	// function that restores the default action; nil keeps the default
	// action throughout. Run calls it once the message is read, or at once
	// for -q: until then the default action is right, as a Ctrl-C while
	// the message is typed must discard it.
	CatchSignals func(ctx context.Context) (context.Context, context.CancelFunc)

	// deliver sends the message to the targets; nil means
	// delivery.Deliver. Tests set it to observe the envelope and the
	// template data and to choose outcomes.
	deliver func(ctx context.Context, targets []delivery.Target, env message.Envelope, d render.Data, files []message.Attachment) []delivery.Result
	// spoolSaved is called after every write of a sidecar during a
	// delivery and after the removal of an entry its last result
	// finished, and entryLocked after a queue run took an entry; tests
	// kill or stop the process there.
	spoolSaved  func(e *spool.Entry)
	entryLocked func(id string)
	// ownEntryCreating is called by deliverOwn before it builds the
	// entry of the own message; tests panic there.
	ownEntryCreating func()
}

// Run handles one invocation and returns the process exit status; args
// are the arguments after argv[0].
//
// Every log record and everything the standard log package prints passes
// through one redactor, which learns the secrets of the configuration
// right after loading it, before the first record that could quote them.
func Run(ctx context.Context, d Deps, args []string, stdin io.Reader) (code int) {
	redactor := &redact.Redactor{}
	d.SetLogOutput(redactor.Writer(d.Stderr))
	// call ties the records of one invocation together: cron sets no
	// Message-ID.
	call := newCallID()
	newLogger := func(tag string) *slog.Logger {
		return slog.New(redactor.Handler(d.NewLogger(tag).Handler())).With("call", call)
	}
	log := newLogger(config.DefaultSyslogTag)
	// rescue holds the message once it is read and until the spool step of
	// the call took it or the call disposed of it; nil otherwise.
	var rescue func()
	// A panic value may quote a request URL; logging it through the
	// redactor keeps the token out, which the runtime's own crash report
	// would not.
	defer func() {
		value := recover()
		if value == nil {
			return
		}
		code = exitSoftware
		// The message is held first, so that a broken log cannot lose it.
		// A second panic, in holding or logging, would make the runtime
		// print the first value unredacted: it is caught, and when the log
		// cannot take its notice either, a constant goes to stderr.
		hasSecondPanic := rescue != nil && callRecovering(rescue)
		hasSecondPanic = callRecovering(func() { log.Error("panic, call ended", panicAttrs(redactor, value)...) }) || hasSecondPanic
		if hasSecondPanic && callRecovering(func() { log.Error("panic while handling a panic") }) {
			_, _ = io.WriteString(d.Stderr, "mailcrier: panic while handling a panic\n")
		}
	}()
	client := *d.HTTP
	if d.ReexecErr != nil {
		// Harden has already replaced the environment; what it cannot
		// undo is what package init read from the caller's one. The HTTP/2
		// debug switch is avoided by speaking HTTP/1.1, and the standard
		// log output is dropped. The process keeps its group, or it could
		// not read the configuration.
		d.SetLogOutput(io.Discard)
		client.Transport = withoutHTTP2(client.Transport)
		log.Error("reexec failed", "err", d.ReexecErr)
	}
	inv, warnings, err := sendmail.Parse(d.Program, args)
	if err != nil {
		// Without syslog the record goes to stderr as well; the line for
		// the user comes last.
		log.Error("command line rejected", "err", err)
		_, _ = fmt.Fprintf(d.Stderr, "mailcrier: %v\n", err)
		return exitUsage
	}
	for _, w := range warnings {
		if w.Message == sendmail.WarningSuppressed {
			log.Warn(w.Message, "count", w.Count)
			continue
		}
		log.Warn(w.Message, "option", w.Option)
	}
	if inv.HasEnvConfigMarker && d.Credentials.isElevated() {
		log.Warn("configuration override ignored", "source", envConfig)
	}
	switch inv.Mode {
	case sendmail.Deliver:
	case sendmail.RunQueue, sendmail.ListQueue, sendmail.Status:
		if inv.Mode == sendmail.RunQueue {
			// mailq and --status print without waiting on anything a
			// signal could cancel.
			var stop context.CancelFunc
			ctx, stop = catchSignals(ctx, d)
			defer stop()
		}
		return runQueueMode(ctx, d, log, newLogger, redactor, &client, inv)
	case sendmail.Probe, sendmail.CheckConfig:
		if code, isAdmitted := admitServiceMode(d, log, inv); !isAdmitted {
			return code
		}
		if inv.Mode == sendmail.CheckConfig {
			return runCheckConfig(ctx, d, log, newLogger, redactor, &client, inv)
		}
		// The probe reads no message, so a stop signal may cancel it at
		// once, killing a hook with its group.
		var stop context.CancelFunc
		ctx, stop = catchSignals(ctx, d)
		defer stop()
		return runProbe(ctx, d, log, newLogger, redactor, &client, inv)
	default:
		return runMode(d, log, inv.Mode)
	}
	configPath, err := selectConfigPath(d, inv, log)
	if err != nil {
		log.Error("configuration rejected, message not delivered", "err", err)
		return exitConfig
	}
	receivedAt := d.Now()
	msg, bcc, readWarnings, err := message.Read(stdin, message.ReadOptions{IgnoreDots: inv.IgnoreDots, MaxSize: message.MaxSize, ReceivedAt: receivedAt})
	if err != nil {
		log.Error("message not read, giving up", "err", err)
		return exitNoInput
	}
	var cfg *config.Config
	var env message.Envelope
	var own *queue
	// received logs the message once; the handler of a panic before the
	// file is loaded logs it too, so that the held message has its record.
	var received func(log *slog.Logger)
	isReceived := false
	rescue = func() {
		if !isReceived && received != nil {
			received(log)
		}
		if own != nil && own.isOwnTaken {
			return
		}
		q, openErr := newQueue(d, log, redactor, spoolSettings(d, cfg), nil)
		_ = q.hold(msg, env, receivedAt, openErr, reasonInternal)
	}
	ctx, stop := catchSignals(ctx, d)
	defer stop()
	env = inv.Envelope(msg, bcc, func() string { return defaultSender(d) })
	// Headers, body and addresses stay out of the log: they may carry
	// anything the calling job printed. The Message-ID, when present,
	// links the records to the message.
	msgAttrs := messageAttrs(msg)
	// callLog is log without the message, for the queue run after it,
	// whose records are about other entries.
	callLog := log
	log = log.With(msgAttrs...)
	// The message is logged under the syslog tag of the file, when it
	// loads: a filter on that tag sees the whole call from here on.
	received = func(log *slog.Logger) {
		isReceived = true
		for _, w := range readWarnings {
			if w == message.WarningTruncated {
				log.Warn(w, "size", msg.Size, "max_size", message.MaxSize)
				continue
			}
			log.Warn(w)
		}
		log.Info("message received", "size", msg.Size, "recipients", len(env.Recipients))
	}
	cfg, err = config.Load(d.ConfigFS, configPath)
	if err != nil {
		received(log)
		// The record of hold that follows says whether the message waits
		// in hold/ for a file that loads or is lost.
		log.Error("configuration rejected", "err", err)
		rescue = nil
		q, openErr := newQueue(d, log, redactor, spoolSettings(d, nil), nil)
		return holdExitCode(q.hold(msg, env, receivedAt, openErr, reasonConfig), exitConfig)
	}
	registerSecrets(redactor, cfg)
	if cfg.General.SyslogTag != config.DefaultSyslogTag {
		callLog = newLogger(cfg.General.SyslogTag)
		log = callLog.With(msgAttrs...)
	}
	received(log)
	client.Timeout = cfg.General.HTTPTimeout.Duration
	targets, err := buildTargets(cfg, &client, hookProcess(d, log))
	if err != nil {
		log.Error("configuration rejected", "err", err)
		rescue = nil
		q, openErr := newQueue(d, log, redactor, spoolSettings(d, cfg), nil)
		return holdExitCode(q.hold(msg, env, receivedAt, openErr, reasonConfig), exitConfig)
	}
	q, openErr := newQueue(d, log, redactor, spoolSettings(d, cfg), targets)
	own = q
	q.router, q.direct = newRouter(cfg), newDirectChats(cfg, targets, &client)
	q.deadline, q.notices = cfg.General.Deadline.Duration, notices(cfg.Strings)
	code = func() int {
		names, v := q.decide(log, msg.Subject, env, false)
		switch v {
		case suppressed:
			return exitOK
		case noRoute:
			log.Warn("no route for message")
			rescue = nil
			return holdExitCode(q.hold(msg, env, receivedAt, openErr, reasonNoRoute), exitUsage)
		}
		data := render.NewData(msg, env, bcc)
		data.Hostname, data.ReceivedAt, data.Strings = d.Hostname, receivedAt, q.notices
		deliverCtx, cancel := withDeadline(ctx, q.deadline)
		defer cancel()
		return q.deliverOwn(deliverCtx, q.selectTargets(names), msg, env, data, openErr)
	}()
	rescue = nil
	// The own message goes first, the caller's queue after it, within its
	// own budget: a failed service must not delay the next message. A
	// panic above skips the queue run.
	q.log = callLog
	q.drainOwn(ctx)
	return code
}

// holdExitCode returns code when the message is held or the spool is off,
// and otherwise 73 or 74, as for a temporary failure without a spool
// entry: the caller must keep a message the spool does not have.
func holdExitCode(state delivery.Queue, code int) int {
	switch state {
	case delivery.QueueNotCreated:
		return exitCantCreate
	case delivery.QueueNotWritten:
		return exitIOErr
	}
	return code
}

// withDeadline bounds the delivery of one message by the deadline key,
// which the error of a target that runs out of it names with its value.
func withDeadline(ctx context.Context, deadline time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeoutCause(ctx, deadline, &backend.LimitError{Key: "deadline", Value: deadline})
}

// catchSignals applies d.CatchSignals to ctx when it is set.
func catchSignals(ctx context.Context, d Deps) (context.Context, context.CancelFunc) {
	if d.CatchSignals == nil {
		return ctx, func() {}
	}
	return d.CatchSignals(ctx)
}

// runQueueMode handles -q, mailq and --status, which need the spool
// directory and, for -q, the targets from the configuration. A file
// that is not read or parsed leaves the default directory, one whose
// targets are rejected its own dir and limits, where a call holds its
// message then: -q only moves expired entries to failed/ there and exits
// 78, mailq and --status list as usual. With a valid file whose dir is not
// the default, -q also releases what the default directory holds.
func runQueueMode(ctx context.Context, d Deps, log *slog.Logger, newLogger func(tag string) *slog.Logger, redactor *redact.Redactor, client *http.Client, inv sendmail.Invocation) int {
	var cfg *config.Config
	var targets []delivery.Target
	configPath, err := selectConfigPath(d, inv, log)
	if err == nil {
		cfg, targets, log, err = loadTargets(d, log, newLogger, redactor, client, configPath, config.Load)
	}
	// parsed is the file even when its targets are rejected: a call holds
	// its message in the dir of the file then.
	parsed := cfg
	if err != nil {
		log.Error("configuration rejected", "err", err)
		cfg, targets = nil, nil
	}
	settings := spoolSettings(d, parsed)
	if inv.Mode == sendmail.ListQueue || inv.Mode == sendmail.Status {
		if err != nil {
			log.Warn("spool listed under a rejected configuration", "dir", settings.Dir)
			tellUser(d, redactor, "spool listed under a rejected configuration", settings.Dir)
		}
		return listSpool(d, log, redactor, settings.Dir, inv.Mode)
	}
	if err != nil {
		tellUser(d, redactor, "configuration rejected", err)
	}
	q, openErr := newQueue(d, log, redactor, settings, targets)
	if cfg != nil {
		q.router, q.direct = newRouter(cfg), newDirectChats(cfg, targets, client)
		q.deadline, q.notices = cfg.General.Deadline.Duration, notices(cfg.Strings)
	}
	if openErr != nil {
		log.Error("spool not opened", "err", openErr)
		tellUser(d, redactor, "spool not opened", openErr)
		if cfg == nil {
			return exitConfig
		}
		return exitIOErr
	}
	// A run on a spool it cannot write would deliver entries and fail to
	// record it, and deliver them again on the next run.
	if q.sp != nil {
		if err := q.sp.CheckWritable(); err != nil {
			log.Error("spool not writable, queue not run", "err", err)
			tellUser(d, redactor, "spool not writable, queue not run", err)
			if cfg == nil {
				return exitConfig
			}
			return exitIOErr
		}
	}
	// A message held while the file was rejected went to the default
	// directory; a file with a dir of its own would never see it.
	if cfg != nil && d.SpoolDir != "" && settings.Dir != d.SpoolDir {
		q.otherHold = d.SpoolDir
	}
	code := q.runQueue(ctx)
	if cfg == nil {
		return exitConfig
	}
	return code
}

// loadTargets loads the configuration at configPath with load, hands its
// secrets to redactor, sets the request timeout of client and builds the
// targets. The logger it returns carries the syslog tag of the
// configuration once it is loaded, log before. When the targets fail, the
// loaded configuration comes back with the error.
func loadTargets(d Deps, log *slog.Logger, newLogger func(tag string) *slog.Logger, redactor *redact.Redactor, client *http.Client, configPath string,
	load func(fs.FS, string) (*config.Config, error),
) (*config.Config, []delivery.Target, *slog.Logger, error) {
	cfg, err := load(d.ConfigFS, configPath)
	if err != nil {
		return nil, nil, log, err
	}
	registerSecrets(redactor, cfg)
	if cfg.General.SyslogTag != config.DefaultSyslogTag {
		log = newLogger(cfg.General.SyslogTag)
	}
	client.Timeout = cfg.General.HTTPTimeout.Duration
	targets, err := buildTargets(cfg, client, hookProcess(d, log))
	return cfg, targets, log, err
}

// listSpool answers mailq, -bp and --status for the spool directory dir
// without creating anything in the spool. A missing directory holds
// nothing for mailq, while --status, read by monitoring, reports it as an
// error.
func listSpool(d Deps, log *slog.Logger, redactor *redact.Redactor, dir string, mode sendmail.Mode) int {
	q := &queue{d: d, log: log, redactor: redactor, settings: config.Spool{Dir: dir}}
	// What -q takes from the hold/ of the default directory into another
	// one is listed with it, see queue.otherHold.
	if d.SpoolDir != "" && dir != d.SpoolDir {
		q.otherHold = d.SpoolDir
	}
	if dir != "" {
		sp, err := spool.OpenExisting(dir)
		switch {
		case err == nil:
			q.sp = sp
		case errors.Is(err, fs.ErrNotExist) && mode == sendmail.ListQueue:
		default:
			log.Error("spool not opened", "err", err)
			tellUser(d, redactor, "spool not opened", err)
			return exitIOErr
		}
	}
	if mode == sendmail.ListQueue {
		return q.listQueue(d.Stdout)
	}
	// Monitoring reads the counts of a spool that takes no new entry as
	// healthy unless the status says otherwise.
	var writeErr error
	if q.sp != nil {
		if writeErr = q.sp.CheckWritable(); writeErr != nil {
			log.Warn("spool not writable", "err", writeErr)
			tellUser(d, redactor, "spool not writable", writeErr)
		}
	}
	if code := q.status(d.Stdout); code != exitOK || writeErr == nil {
		return code
	}
	return exitIOErr
}

// tellUser writes the line on stderr that tells the caller of -q, mailq or
// --status what failed, as a usage error does, interactive or not: a
// monitoring script reads stderr too, and the record in the mail log
// carries the rest. An elevated caller gets the constant message alone, as
// from the log without syslog; nothing is written when the log goes to
// stderr already. detail, an error or a path, is redacted.
func tellUser(d Deps, redactor *redact.Redactor, message string, detail any) {
	if d.IsLogOnStderr != nil && d.IsLogOnStderr() {
		return
	}
	line := "mailcrier: " + message
	if detail != nil && !d.Credentials.isElevated() {
		line += ": " + redactor.String(fmt.Sprint(detail))
	}
	_, _ = fmt.Fprintln(d.Stderr, line)
}

// notices returns the built-in notices with the configured ones in place.
// A configured truncated without truncated_size stands for both: the
// English default with the size would replace a translated notice.
func notices(configured config.Strings) render.Strings {
	strs := render.DefaultStrings()
	for _, notice := range []struct {
		value string
		dst   *string
	}{
		{configured.NoSubject, &strs.NoSubject}, {configured.EmptyBody, &strs.EmptyBody}, {configured.Truncated, &strs.Truncated},
		{configured.TruncatedSize, &strs.TruncatedSize}, {configured.MoreAttachments, &strs.MoreAttachments}, {configured.NotSent, &strs.NotSent},
	} {
		if notice.value != "" {
			*notice.dst = notice.value
		}
	}
	if configured.Truncated != "" && configured.TruncatedSize == "" {
		strs.TruncatedSize = ""
	}
	return strs
}

// defaultSender returns the sender of a message without -f and From. An
// elevated process takes the login name of its real uid from the user
// database, because USER, LOGNAME and EMAIL are the caller's to set;
// otherwise EMAIL wins, then USER or LOGNAME at the host name. Empty when
// nothing is known.
func defaultSender(d Deps) string {
	if !d.Credentials.isElevated() {
		if email, ok := lookupEnv(d.Environ, "EMAIL"); ok && email != "" {
			return email
		}
		for _, key := range []string{"USER", "LOGNAME"} {
			if name, ok := lookupEnv(d.Environ, key); ok && name != "" {
				return name + "@" + d.Hostname
			}
		}
	}
	if name, ok := d.LookupUserName(d.Credentials.UID); ok {
		return name + "@" + d.Hostname
	}
	return ""
}

// Reasons of the record of a message lost after temporary failures, by
// the state of its spool entry.
var lostReasons = map[delivery.Queue]string{
	delivery.QueueOff: "spool off", delivery.QueueNotCreated: "spool entry not created", delivery.QueueNotWritten: "spool entry not written",
}

// logOutcome adds the records for the message as a whole: why it is not
// in the spool, spoolErr, what was queued for a later attempt and what
// was lost. A message lost is one record, "message lost" with the cause,
// without "message not spooled" or "message not delivered" besides.
func logOutcome(log *slog.Logger, results []delivery.Result, state delivery.Queue, spoolErr error) {
	var delivered, temporary, rejected []string
	for _, r := range results {
		switch r.Status {
		case delivery.OK:
			delivered = append(delivered, r.TargetID)
		case delivery.Temp:
			temporary = append(temporary, r.TargetID)
		default:
			rejected = append(rejected, r.TargetID)
		}
	}
	isAllTemporary := len(temporary) > 0 && len(temporary) == len(results)
	reason := lostReasons[state]
	if errors.Is(spoolErr, spool.ErrOpen) {
		reason = "spool not opened"
	}
	isLost := state != delivery.Queued && isAllTemporary && len(delivered) == 0
	if spoolErr != nil && !isLost {
		// Without a temporary failure nothing needed the entry. The error
		// tells a directory not opened from an entry not created or not
		// written.
		level := slog.LevelWarn
		if len(temporary) > 0 {
			level = slog.LevelError
		}
		log.Log(context.Background(), level, "message not spooled", "err", spoolErr)
	}
	switch {
	case state == delivery.Queued && isAllTemporary:
		log.Warn("message queued", "targets", temporary)
	case state == delivery.Queued:
		for _, name := range temporary {
			log.Warn("message queued for target", "target", name)
		}
	case len(delivered) > 0 || (state != delivery.QueueOff && !isAllTemporary):
		for _, name := range temporary {
			log.Error("message lost for target", "target", name, "reason", reason)
		}
	case isAllTemporary:
		attrs := []any{"reason", reason, "targets", temporary}
		if spoolErr != nil {
			attrs = append(attrs, "err", spoolErr)
		}
		log.Error("message lost", attrs...)
		return
	}
	if len(delivered) == 0 && delivery.ExitCode(results, state) == exitUnavailable {
		log.Error("message not delivered", "targets", rejected)
	}
}

// runMode answers the modes that read no message.
func runMode(d Deps, log *slog.Logger, mode sendmail.Mode) int {
	switch mode {
	case sendmail.NewAliases:
		return exitOK
	case sendmail.Version:
		_, _ = fmt.Fprintln(d.Stdout, "mailcrier", buildVersion())
		return exitOK
	case sendmail.Help:
		_, _ = io.WriteString(d.Stdout, sendmail.Usage)
		return exitOK
	default:
		// A mode that Run does not dispatch is a bug, not a request for
		// help.
		log.Error("unknown mode", "mode", int(mode))
		return exitSoftware
	}
}

// buildVersion returns the module version stamped into the binary.
func buildVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		return info.Main.Version
	}
	return "(devel)"
}

// maxMessageIDLength bounds the msgid field: the header comes from the
// caller and is repeated in every record of the call.
const maxMessageIDLength = 256

// messageAttrs returns the msgid field, cut to maxMessageIDLength, or
// nothing when the message has no Message-ID.
func messageAttrs(msg *message.Message) []any {
	id := msg.MessageID
	if id == "" {
		return nil
	}
	if len(id) > maxMessageIDLength {
		id = strings.ToValidUTF8(id[:maxMessageIDLength], "")
	}
	return []any{"msgid", id}
}

// newCallID returns 8 random bytes in hex.
func newCallID() string {
	id := make([]byte, 8)
	_, _ = rand.Read(id) // never fails on Linux, per crypto/rand
	return hex.EncodeToString(id)
}

// selectConfigPath returns the configuration file relative to d.ConfigFS:
// --config wins over MAILCRIER_CONFIG, which wins over the default. An
// elevated process ignores both, with a warning: they would let any user
// read an arbitrary file, or send the message to a receiver of their own,
// with the group privilege.
func selectConfigPath(d Deps, inv sendmail.Invocation, log *slog.Logger) (string, error) {
	source, path := "", ""
	if value, ok := lookupEnv(d.Environ, envConfig); ok && value != "" {
		source, path = envConfig, value
	}
	if inv.ConfigPath != "" {
		source, path = sendmail.OptionConfig, inv.ConfigPath
	}
	switch {
	case source == "":
		return d.ConfigPath, nil
	case d.Credentials.isElevated():
		log.Warn("configuration override ignored", "source", source)
		return d.ConfigPath, nil
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("%s: %w", source, err)
	}
	return strings.TrimPrefix(absolute, "/"), nil
}

// logResult records the outcome of one target. For a failure class is
// temp or perm, status the HTTP status when the service answered,
// retry_after the delay it asked for and, after a panic, stack the stack
// of the goroutine, redacted with redactor. A temporary failure the spool
// retries, isQueued, is a warning: nobody needs to act unless it lasts.
func logResult(log *slog.Logger, redactor *redact.Redactor, r delivery.Result, isQueued bool) {
	if r.IsTruncated {
		log.Info("text truncated for target", "target", r.TargetID)
	}
	if r.IsOverOutput {
		// The text was cut because the template wrote too much, not because
		// the message is long: the template needs fixing.
		log.Warn("template output limit reached", "target", r.TargetID)
	}
	if r.TemplateErr != nil {
		log.Warn("template failed, built-in used", "target", r.TargetID, "err", r.TemplateErr)
	}
	switch {
	case r.TextRejected != nil && r.Status == delivery.OK:
		log.Warn("text rejected, sent as file", "target", r.TargetID, "err", r.TextRejected)
	case r.TextRejected != nil:
		log.Warn("text rejected, file failed too", "target", r.TargetID, "err", r.TextRejected)
	}
	// A request part has no built-in template to fall back to: the
	// target gets nothing, which the warning names as the cause.
	if r.RequestErr != nil {
		log.Warn("request template failed, message not sent", "target", r.TargetID, "err", r.RequestErr)
		return
	}
	switch {
	case r.Status == delivery.OK && r.Err != nil:
		log.Warn("attachments not delivered", "target", r.TargetID, "err", r.Err)
		return
	case r.Status == delivery.OK:
		log.Debug("target delivered", "target", r.TargetID)
		return
	}
	attrs := []any{"target", r.TargetID, "class", r.Status}
	var deliveryErr *backend.Error
	if errors.As(r.Err, &deliveryErr) && deliveryErr.Status != 0 {
		attrs = append(attrs, "status", deliveryErr.Status)
	}
	if deliveryErr != nil && deliveryErr.RetryAfter > 0 {
		attrs = append(attrs, "retry_after", deliveryErr.RetryAfter)
	}
	attrs = append(attrs, "err", failureCause(r.Err))
	var panicErr *backend.PanicError
	if errors.As(r.Err, &panicErr) {
		attrs = append(attrs, "stack", stackText(redactor, panicErr.Stack))
	}
	if r.Status == delivery.Temp && isQueued {
		log.Warn("target failed, retry queued", attrs...)
		return
	}
	log.Error("target failed", attrs...)
}

// failureCause returns the cause of the failure err of a target without
// the class and the status of a *backend.Error, which the record carries
// as fields: "Service Unavailable", not "temporary failure, status 503:
// Service Unavailable". Any other error comes back as it is.
func failureCause(err error) error {
	if deliveryErr, ok := err.(*backend.Error); ok && deliveryErr.Err != nil {
		return deliveryErr.Err
	}
	return err
}

// minHeaderSecret is the shortest header value, or credential after an
// authorization scheme, that registerSecrets masks: masking a value such
// as "1" would corrupt every number in the log, and a credential is
// longer.
const minHeaderSecret = 8

// registerSecrets hands every value that may hold a secret to the redactor:
// tokens and URLs, including those read from *_file, and the values of
// extra HTTP headers, such as Authorization, with the credential after
// the scheme ("Bearer <token>") on its own as well. Header and query
// values and the path are templates: what they write as it is, outside
// the {{ }} actions, is registered the same way, and so is every fragment
// of such path text between slashes, as written and unescaped, from
// minPathSecret characters on; what the actions render comes from the
// message and is never logged. config.SecretKeys lists the keys covered
// here and grows with this function: a file that writes one of them out is
// one --check-config warns about.
func registerSecrets(redactor *redact.Redactor, cfg *config.Config) {
	for _, name := range cfg.TargetNames() {
		target := cfg.Targets[name]
		redactor.Add(target.Token)
		redactor.AddURL(target.URL)
		for _, source := range slices.Concat(slices.Collect(maps.Values(target.Headers)), slices.Collect(maps.Values(target.Query))) {
			for _, value := range templateText(source) {
				secrets := []string{strings.TrimSpace(value)}
				if fields := strings.Fields(value); len(fields) > 1 {
					secrets = append(secrets, fields[len(fields)-1])
				}
				for _, secret := range secrets {
					if len(secret) >= minHeaderSecret {
						redactor.Add(secret)
					}
				}
			}
		}
		for _, value := range templateText(target.Path) {
			for _, fragment := range strings.Split(value, "/") {
				secrets := []string{fragment}
				if unescaped, err := url.PathUnescape(fragment); err == nil {
					secrets = append(secrets, unescaped)
				}
				for _, secret := range secrets {
					if len(secret) >= minPathSecret {
						redactor.Add(secret)
					}
				}
			}
		}
	}
}

// minPathSecret is the shortest fragment of the path of an http target
// that registerSecrets masks, the length from which redact.AddURL masks a
// segment of a URL: a shorter one is a word such as "api" or "v1".
const minPathSecret = 16

// templateAction matches a {{ }} action of a template, as far as the
// first }}; an action that holds "}}" in a string is cut there, which
// only registers a little more text as a secret.
var templateAction = regexp.MustCompile(`(?s)\{\{.*?\}\}`)

// templateText returns the parts of a template source outside its
// actions; the source itself when it has none.
func templateText(source string) []string {
	return templateAction.Split(source, -1)
}

// presetFormats maps the presets of the http target to their built-in
// templates.
var presetFormats = map[string]text.Format{
	config.PresetMattermost: text.FormatMattermost, config.PresetSlackWebhook: text.FormatSlackWebhook, config.PresetGenericJSON: text.FormatGenericJSON,
}

// hookProcess returns what the exec targets of an invocation share. A
// process with a group its caller does not have, that of the setgid
// binary, starts a hook with the real ids, so that the hook cannot read
// the configuration and the spool; root keeps its ids either way.
func hookProcess(d Deps, log *slog.Logger) hook.Process {
	process := hook.Process{Log: log}
	process.TZ, _ = lookupEnv(d.Environ, "TZ")
	if c := d.Credentials; c.EGID != c.GID {
		process.Credential = &syscall.Credential{Uid: uint32(c.UID), Gid: uint32(c.GID), NoSetGroups: true}
	}
	return process
}

// buildTargets maps configured targets to senders and templates, in name
// order so that logs do not depend on map iteration. All targets share
// client, which carries the request timeout, and the exec targets hooks.
// An error is the *config.Error at the key of the target that failed; a
// template from template_file is named by the path of the file, so that
// the line in its error is a line of that file.
func buildTargets(cfg *config.Config, client *http.Client, hooks hook.Process) ([]delivery.Target, error) {
	var targets []delivery.Target
	for _, name := range cfg.TargetNames() {
		target := cfg.Targets[name]
		var sender backend.Sender
		var format text.Format
		var request *delivery.RequestTemplates
		switch target.Type {
		case config.TypeTelegram:
			sender, format = telegram.New(telegramOptions(target, client)), text.FormatTelegramHTML
		case config.TypeDiscord:
			sender, format = discord.New(discord.Options{URL: target.URL, Client: client}), text.FormatDiscord
		case config.TypeSlack:
			sender = slack.New(slack.Options{Token: target.Token, Channel: target.Channel, Client: client})
			format = text.FormatSlackMrkdwn
		case config.TypeNtfy:
			topic, err := ntfy.New(ntfy.Options{URL: target.URL, Client: client})
			if err != nil {
				return nil, cfg.TargetError(name, err, "url", "url_file")
			}
			sender, format = topic, text.FormatNtfy
		case config.TypeHTTP:
			fields := map[string]string{}
			for key, value := range map[string]string{"username": target.Username, "channel": target.Channel} {
				if value != "" {
					fields[key] = value
				}
			}
			// A template without preset writes a JSON body, which Fit
			// cuts as strictly as that of generic-json.
			format = text.FormatGenericJSON
			if target.Preset != "" {
				format = presetFormats[target.Preset]
			}
			sender = webhook.New(webhook.Options{URL: target.URL, Method: target.Method, Format: format, Fields: fields, Client: client})
			var err error
			if request, err = parseRequest(cfg, name, target); err != nil {
				return nil, err
			}
		case config.TypeExec:
			// The hook gets the message, not the text; the plain template
			// has no limit and nothing to escape, so rendering it is cheap
			// and cannot fail on the message.
			sender = hook.New(hook.Options{Name: name, Argv: target.Argv, Timeout: target.Timeout.Duration, Process: hooks})
			format = text.FormatPlain
		case config.TypeShoutrrr:
			service, err := shoutrrr.New(shoutrrr.Options{URL: target.URL, Client: client})
			if err != nil {
				return nil, cfg.TargetError(name, err, "url", "url_file")
			}
			sender, format = service, text.FormatPlain
		default:
			return nil, cfg.TargetError(name, fmt.Errorf("type %q is not implemented", target.Type), "type")
		}
		builtin, err := render.Builtin(format)
		if err != nil {
			return nil, cfg.TargetError(name, err)
		}
		tmpl, fallback := builtin, (*render.Template)(nil)
		switch {
		case target.Type == config.TypeHTTP && target.Method == http.MethodGet:
			// A GET carries no body, so there is no text to render.
			tmpl = nil
		case target.Template != "":
			templateName := name
			if target.TemplateFile != "" {
				templateName = target.TemplateFile
			}
			if tmpl, err = render.Parse(templateName, target.Template, format); err != nil {
				return nil, cfg.TargetError(name, err, "template", "template_file")
			}
			fallback = builtin
		}
		targets = append(targets, delivery.Target{
			ID: name, Sender: sender, Template: tmpl, Fallback: fallback, Request: request, OnLong: onLongPolicies[target.OnLong], LongFile: longFiles[target.LongFile],
			MaxText: target.MaxText, MaxLines: target.MaxLines, MaxFileSize: target.MaxFileSize,
		})
	}
	return targets, nil
}

// telegramOptions returns the options of the sender of a telegram target;
// the copies for direct chats start from them as well.
func telegramOptions(target config.Target, client *http.Client) telegram.Options {
	return telegram.Options{
		Token: target.Token, ChatID: target.ChatID, MessageThreadID: target.MessageThreadID,
		DisableNotification: target.DisableNotification, Client: client,
	}
}

// parseRequest parses the templates of the path, query and headers of an
// http target of cfg; nil when it has none. A template is named after the
// target and its key, such as "api.headers.Authorization", which the
// errors quote with the line in the template; the error is at the key in
// the file.
func parseRequest(cfg *config.Config, name string, target config.Target) (*delivery.RequestTemplates, error) {
	if target.Path == "" && len(target.Query) == 0 && len(target.Headers) == 0 {
		return nil, nil
	}
	// The path of the key in the file is kept in parts: a header name may
	// hold a dot.
	parse := func(source string, key ...string) (*render.Template, error) {
		tmpl, err := render.ParsePart(name+"."+strings.Join(key, "."), source)
		if err != nil {
			return nil, cfg.TargetPathError(name, err, key...)
		}
		return tmpl, nil
	}
	request := &delivery.RequestTemplates{Query: map[string]*render.Template{}, Headers: map[string]*render.Template{}}
	var err error
	if target.Path != "" {
		if request.Path, err = parse(target.Path, "path"); err != nil {
			return nil, err
		}
	}
	for key, source := range target.Query {
		if request.Query[key], err = parse(source, "query", key); err != nil {
			return nil, err
		}
	}
	for key, source := range target.Headers {
		if request.Headers[key], err = parse(source, "headers", key); err != nil {
			return nil, err
		}
	}
	return request, nil
}

// onLongPolicies and longFiles map the values of on_long and long_file;
// an absent key, the empty string, gives the zero value, the default.
var (
	onLongPolicies = map[string]delivery.OnLong{
		config.OnLongFile: delivery.OnLongFile, config.OnLongTruncate: delivery.OnLongTruncate, config.OnLongBlockquote: delivery.OnLongBlockquote,
	}
	longFiles = map[string]delivery.LongFile{config.LongFileText: delivery.LongFileText, config.LongFileMessage: delivery.LongFileMessage}
)
