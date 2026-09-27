package app

import (
	"bytes"
	"context"
	"log/slog"
	"log/syslog"
	"net/http"
	"os"
	"sync"
)

// SystemConfigPath is the configuration file, relative to the root of
// SystemDeps().ConfigFS.
const SystemConfigPath = "etc/slendmail.conf"

// SystemDeps returns the Deps of a real invocation: syslog, the root file
// system and the default HTTP transport.
func SystemDeps() Deps {
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "unknown"
	}
	return Deps{
		NewLogger:  newSystemLogger,
		ConfigFS:   os.DirFS("/"),
		ConfigPath: SystemConfigPath,
		HTTP:       &http.Client{},
		Hostname:   hostname,
	}
}

// newSystemLogger logs to syslog with the mail facility, because cron and
// at discard the output of the mailer they run. When syslog is unreachable
// the records go to stderr instead of being lost.
func newSystemLogger(tag string) *slog.Logger {
	writer, err := syslog.New(syslog.LOG_MAIL|syslog.LOG_INFO, tag)
	if err != nil {
		logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
		logger.Warn("syslog unavailable, logging to stderr", "err", err)
		return logger
	}
	return slog.New(newSyslogHandler(writer))
}

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
