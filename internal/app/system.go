package app

import (
	"bytes"
	"context"
	"io"
	"log"
	"log/slog"
	"log/syslog"
	"net"
	"net/http"
	"os"
	"os/user"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/6RUN0/mailcrier/internal/config"
)

// SystemConfigPath is the configuration file, relative to the root of
// SystemDeps().ConfigFS.
const SystemConfigPath = "etc/mailcrier.conf"

// serviceUser is the system user that runs the queue and owns the
// configuration together with root.
const serviceUser = "mailcrier"

// CurrentProcess returns what Harden needs, without reading anything but
// the command line, the environment and the ids.
func CurrentProcess() Process {
	return Process{
		Argv:        os.Args,
		Environ:     os.Environ(),
		Credentials: Credentials{UID: os.Getuid(), GID: os.Getgid(), EGID: os.Getegid()},
		Exec:        syscall.Exec,
		ReplaceEnv:  replaceEnv,
	}
}

// SystemDeps returns the Deps of a real invocation, to be called after
// Harden: syslog, the root file system, the ids and environment of the
// process, and an HTTP transport.
func SystemDeps() Deps {
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "unknown"
	}
	creds := Credentials{
		UID:        os.Getuid(),
		GID:        os.Getgid(),
		EGID:       os.Getegid(),
		ServiceUID: lookupServiceUID(),
	}
	fallback := newStderrLog(os.Stderr, creds.isElevated())
	return Deps{
		NewLogger: func(tag string) *slog.Logger {
			return newFallbackLogger(tag, dialSyslog, fallback)
		},
		IsLogOnStderr:  fallback.isDown,
		ConfigFS:       os.DirFS("/"),
		ConfigPath:     SystemConfigPath,
		HTTP:           &http.Client{Transport: newTransport(creds.isElevated())},
		Hostname:       hostname,
		Now:            time.Now,
		Program:        programName(os.Args),
		Stdout:         os.Stdout,
		Stderr:         os.Stderr,
		SetLogOutput:   log.SetOutput,
		Credentials:    creds,
		Environ:        os.Environ(),
		LookupUserName: lookupUserName,
		SpoolDir:       config.DefaultSpoolDir,
	}
}

// programName returns argv[0] of argv, installedPath for an empty argv as
// Harden takes it.
func programName(argv []string) string {
	if len(argv) == 0 {
		return installedPath
	}
	return argv[0]
}

// lookupUserName returns the login name of uid from the user database.
func lookupUserName(uid int) (string, bool) {
	account, err := user.LookupId(strconv.Itoa(uid))
	if err != nil {
		return "", false
	}
	return account.Username, true
}

// newTransport returns the HTTP transport of a real invocation. Without
// elevation it honours the proxy variables, which a container behind a
// proxy needs. An elevated process uses no proxy: the variables are the
// caller's and would route token-bearing requests through a host of the
// caller's choice.
func newTransport(isElevated bool) *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = http.ProxyFromEnvironment
	if isElevated {
		transport.Proxy = nil
	}
	return transport
}

// lookupServiceUID returns the uid of the mailcrier user, -1 when the
// system has none.
func lookupServiceUID() int {
	account, err := user.Lookup(serviceUser)
	if err != nil {
		return -1
	}
	uid, err := strconv.Atoi(account.Uid)
	if err != nil {
		return -1
	}
	return uid
}

// replaceEnv clears the environment and sets env. os.Clearenv also resets
// the settings the runtime took from GODEBUG.
func replaceEnv(env []string) {
	os.Clearenv()
	for _, entry := range env {
		if key, value, ok := strings.Cut(entry, "="); ok {
			_ = os.Setenv(key, value)
		}
	}
}

// syslogSocket is the socket dialSyslog tries first, as log/syslog does.
const syslogSocket = "/dev/log"

// dialSyslog connects to syslog with the mail facility, because cron and
// at discard the output of the mailer they run. log/syslog reports any
// failure as "Unix syslog delivery error"; a dial of its first socket
// names the path and the errno instead.
func dialSyslog(tag string) (*syslog.Writer, error) {
	writer, err := syslog.New(syslog.LOG_MAIL|syslog.LOG_INFO, tag)
	if err != nil {
		conn, dialErr := net.Dial("unixgram", syslogSocket)
		if dialErr != nil {
			return nil, dialErr
		}
		_ = conn.Close()
	}
	return writer, err
}

// newFallbackLogger logs to the syslog writer dial returns or, when syslog
// is unreachable (a container has no /dev/log), to fallback, so that the
// records are not lost and the call goes on. Once syslog failed, by a
// dial or a write, the loggers made after it do not dial again.
func newFallbackLogger(tag string, dial func(tag string) (*syslog.Writer, error), fallback *stderrLog) *slog.Logger {
	if !fallback.isDown() {
		writer, err := dial(tag)
		if err == nil {
			return slog.New(newSyslogHandler(writer, fallback))
		}
		fallback.setDown("syslog unavailable, logging to stderr", err)
	}
	return slog.New(fallback.handler())
}

// stderrLog takes the records when syslog is unreachable or stops taking
// them; one per process, so that the warning about it comes once, before
// the first record on stderr and with its fields, such as call. Without
// elevation stderr gets logfmt with time in UTC and level. An elevated
// process writes for its caller, who must not see what only the group may
// read (configuration positions, target names, statuses), so stderr gets
// only the constant message of warnings and errors.
type stderrLog struct {
	base slog.Handler
	mu   sync.Mutex
	down bool
	// warning is the message of the warning not yet written, cause its
	// error; empty once written.
	warning string
	cause   error
}

func newStderrLog(stderr io.Writer, isElevated bool) *stderrLog {
	l := &stderrLog{}
	if isElevated {
		l.base = &messageHandler{writer: stderr, mu: &sync.Mutex{}}
		return l
	}
	l.base = slog.NewTextHandler(stderr, &slog.HandlerOptions{
		Level: slog.LevelDebug,
		ReplaceAttr: func(groups []string, attr slog.Attr) slog.Attr {
			if len(groups) == 0 && attr.Key == slog.TimeKey {
				return slog.Time(slog.TimeKey, attr.Value.Time().UTC())
			}
			return attr
		},
	})
	return l
}

// isDown reports whether the records go to stderr.
func (l *stderrLog) isDown() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.down
}

// setDown sends every later record to stderr; the first call sets the
// warning that comes before them.
func (l *stderrLog) setDown(warning string, cause error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.down {
		l.down, l.warning, l.cause = true, warning, cause
	}
}

func (l *stderrLog) handler() *stderrHandler {
	return &stderrHandler{log: l, handler: l.base}
}

// stderrHandler writes records with the handler of a stderrLog, derived
// with the attributes of its logger, after the pending warning.
type stderrHandler struct {
	log     *stderrLog
	handler slog.Handler
}

// Enabled defers to the handler on stderr.
func (h *stderrHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.handler.Enabled(ctx, level)
}

// Handle writes the pending warning, then the record when the handler on
// stderr takes its level: a syslogHandler passes on every level.
func (h *stderrHandler) Handle(ctx context.Context, record slog.Record) error {
	h.log.mu.Lock()
	defer h.log.mu.Unlock()
	if h.log.warning != "" {
		warning := slog.NewRecord(record.Time, slog.LevelWarn, h.log.warning, 0)
		warning.AddAttrs(slog.Any("err", h.log.cause))
		h.log.warning = ""
		if err := h.handler.Handle(ctx, warning); err != nil {
			return err
		}
	}
	if !h.handler.Enabled(ctx, record.Level) {
		return nil
	}
	return h.handler.Handle(ctx, record)
}

// WithAttrs returns a handler that adds attrs to every record.
func (h *stderrHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &stderrHandler{log: h.log, handler: h.handler.WithAttrs(attrs)}
}

// WithGroup returns a handler that nests later attributes under name.
func (h *stderrHandler) WithGroup(name string) slog.Handler {
	return &stderrHandler{log: h.log, handler: h.handler.WithGroup(name)}
}

// messageHandler writes "mailcrier: <message>" for warnings and errors and
// drops every attribute. Messages are constants, so they carry no data.
type messageHandler struct {
	writer io.Writer
	mu     *sync.Mutex
}

// Enabled accepts warnings and errors.
func (h *messageHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= slog.LevelWarn
}

// Handle writes the message alone.
func (h *messageHandler) Handle(_ context.Context, record slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := io.WriteString(h.writer, "mailcrier: "+record.Message+"\n")
	return err
}

// WithAttrs drops attrs.
func (h *messageHandler) WithAttrs([]slog.Attr) slog.Handler { return h }

// WithGroup drops the group.
func (h *messageHandler) WithGroup(string) slog.Handler { return h }

// syslogHandler formats records as logfmt without time and level, which
// syslog records itself, and sends each one with the matching severity.
// A record syslog does not take goes to fallback, and so does every one
// after it: a daemon that stopped would lose the records without a trace,
// and a dial per record would slow every one down.
type syslogHandler struct {
	writer *syslog.Writer
	// format renders into *buf; mu guards buf, which handlers derived with
	// WithAttrs and WithGroup share.
	format slog.Handler
	buf    *bytes.Buffer
	mu     *sync.Mutex
	// fallback is the handler on stderr, derived with the same attributes.
	fallback *stderrHandler
}

func newSyslogHandler(writer *syslog.Writer, fallback *stderrLog) *syslogHandler {
	buf := &bytes.Buffer{}
	format := slog.NewTextHandler(buf, &slog.HandlerOptions{
		Level: slog.LevelDebug,
		ReplaceAttr: func(groups []string, attr slog.Attr) slog.Attr {
			if len(groups) == 0 && (attr.Key == slog.TimeKey || attr.Key == slog.LevelKey) {
				return slog.Attr{}
			}
			return attr
		},
	})
	return &syslogHandler{writer: writer, format: format, buf: buf, mu: &sync.Mutex{}, fallback: fallback.handler()}
}

// Enabled accepts every level; the syslog daemon filters by severity.
func (h *syslogHandler) Enabled(context.Context, slog.Level) bool {
	return true
}

// Handle sends one record to syslog, or to stderr once syslog failed.
func (h *syslogHandler) Handle(ctx context.Context, record slog.Record) error {
	if h.fallback.log.isDown() {
		return h.fallback.Handle(ctx, record)
	}
	err := h.send(ctx, record)
	if err == nil {
		return nil
	}
	h.fallback.log.setDown("syslog write failed, logging to stderr", err)
	return h.fallback.Handle(ctx, record)
}

// send formats one record and writes it to syslog; log/syslog dials once
// more before it gives up.
func (h *syslogHandler) send(ctx context.Context, record slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.buf.Reset()
	if err := h.format.Handle(ctx, record); err != nil {
		return err
	}
	line := string(bytes.TrimSuffix(h.buf.Bytes(), []byte("\n")))
	switch {
	case record.Level >= slog.LevelError:
		return h.writer.Err(line)
	case record.Level >= slog.LevelWarn:
		return h.writer.Warning(line)
	case record.Level >= slog.LevelInfo:
		return h.writer.Info(line)
	default:
		return h.writer.Debug(line)
	}
}

// WithAttrs returns a handler that adds attrs to every record.
func (h *syslogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	derived := *h
	derived.format = h.format.WithAttrs(attrs)
	derived.fallback = h.fallback.WithAttrs(attrs).(*stderrHandler)
	return &derived
}

// WithGroup returns a handler that nests later attributes under name.
func (h *syslogHandler) WithGroup(name string) slog.Handler {
	derived := *h
	derived.format = h.format.WithGroup(name)
	derived.fallback = h.fallback.WithGroup(name).(*stderrHandler)
	return &derived
}
