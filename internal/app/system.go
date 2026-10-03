package app

import (
	"bytes"
	"context"
	"io"
	"log"
	"log/slog"
	"log/syslog"
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
const SystemConfigPath = "etc/slendmail.conf"

// serviceUser is the system user that runs the queue and owns the
// configuration together with root.
const serviceUser = "slendmail"

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
	return Deps{
		NewLogger: func(tag string) *slog.Logger {
			return newFallbackLogger(tag, dialSyslog, os.Stderr, creds.isElevated())
		},
		ConfigFS:       os.DirFS("/"),
		ConfigPath:     SystemConfigPath,
		HTTP:           &http.Client{Transport: newTransport(creds.isElevated())},
		Hostname:       hostname,
		Now:            time.Now,
		Program:        os.Args[0],
		Stdout:         os.Stdout,
		Stderr:         os.Stderr,
		SetLogOutput:   log.SetOutput,
		Credentials:    creds,
		Environ:        os.Environ(),
		LookupUserName: lookupUserName,
		SpoolDir:       config.DefaultSpoolDir,
	}
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

// lookupServiceUID returns the uid of the slendmail user, -1 when the
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

// dialSyslog connects to syslog with the mail facility, because cron and
// at discard the output of the mailer they run.
func dialSyslog(tag string) (*syslog.Writer, error) {
	return syslog.New(syslog.LOG_MAIL|syslog.LOG_INFO, tag)
}

// newFallbackLogger logs to the syslog writer dial returns or, when syslog
// is unreachable (a container has no /dev/log), to stderr, so that the
// records are not lost and the call goes on. Without elevation stderr gets
// logfmt with time and level. An elevated process writes for its caller,
// who must not see what only the group may read (configuration positions,
// target names, statuses), so stderr gets only the constant message of
// warnings and errors.
func newFallbackLogger(tag string, dial func(tag string) (*syslog.Writer, error), stderr io.Writer, isElevated bool) *slog.Logger {
	writer, err := dial(tag)
	if err == nil {
		return slog.New(newSyslogHandler(writer))
	}
	var logger *slog.Logger
	if isElevated {
		logger = slog.New(&messageHandler{writer: stderr, mu: &sync.Mutex{}})
	} else {
		logger = slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	logger.Warn("syslog unavailable, logging to stderr", "err", err)
	return logger
}

// messageHandler writes "slendmail: <message>" for warnings and errors and
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
	_, err := io.WriteString(h.writer, "slendmail: "+record.Message+"\n")
	return err
}

// WithAttrs drops attrs.
func (h *messageHandler) WithAttrs([]slog.Attr) slog.Handler { return h }

// WithGroup drops the group.
func (h *messageHandler) WithGroup(string) slog.Handler { return h }

// syslogHandler formats records as logfmt without time and level, which
// syslog records itself, and sends each one with the matching severity.
type syslogHandler struct {
	writer *syslog.Writer
	// format renders into *buf; mu guards buf, which handlers derived with
	// WithAttrs and WithGroup share.
	format slog.Handler
	buf    *bytes.Buffer
	mu     *sync.Mutex
}

func newSyslogHandler(writer *syslog.Writer) *syslogHandler {
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
	return &syslogHandler{writer: writer, format: format, buf: buf, mu: &sync.Mutex{}}
}

// Enabled accepts every level; the syslog daemon filters by severity.
func (h *syslogHandler) Enabled(context.Context, slog.Level) bool {
	return true
}

// Handle sends one record to syslog.
func (h *syslogHandler) Handle(ctx context.Context, record slog.Record) error {
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
	return &derived
}

// WithGroup returns a handler that nests later attributes under name.
func (h *syslogHandler) WithGroup(name string) slog.Handler {
	derived := *h
	derived.format = h.format.WithGroup(name)
	return &derived
}
