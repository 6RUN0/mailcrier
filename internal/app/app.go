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
	"net/http"
	"strings"

	"github.com/6RUN0/slendmail/internal/backend"
	"github.com/6RUN0/slendmail/internal/backend/webhook"
	"github.com/6RUN0/slendmail/internal/config"
	"github.com/6RUN0/slendmail/internal/delivery"
	"github.com/6RUN0/slendmail/internal/message"
	"github.com/6RUN0/slendmail/internal/redact"
)

// Exit statuses from sysexits.h that Run produces itself.
const (
	exitNoInput  = 66
	exitSoftware = 70
	exitConfig   = 78
)

// Deps are the parts of the environment an invocation uses.
type Deps struct {
	// NewLogger returns the logger for a syslog tag. Run calls it once with
	// the default tag and again when the configuration sets another one.
	NewLogger func(tag string) *slog.Logger
	// ConfigFS and ConfigPath locate the configuration file; secret files
	// named in it are read from ConfigFS too.
	ConfigFS   fs.FS
	ConfigPath string
	// HTTP performs the requests of HTTP-based targets. Run uses a copy
	// with the configured request timeout.
	HTTP *http.Client
	// Hostname is the name of the machine, sent along with each message.
	Hostname string
	// Stderr receives what the standard log package writes: net/http and
	// its HTTP/2 transport print their debug output there.
	Stderr io.Writer
	// SetLogOutput redirects the standard log package, as log.SetOutput.
	SetLogOutput func(w io.Writer)
}

// Run handles one invocation and returns the process exit status. Command
// line arguments are accepted and ignored.
//
// Every log record and everything the standard log package prints passes
// through one redactor, which learns the secrets of the configuration
// right after loading it, before the first record that could quote them.
func Run(ctx context.Context, d Deps, _ []string, stdin io.Reader) (code int) {
	redactor := &redact.Redactor{}
	d.SetLogOutput(redactor.Writer(d.Stderr))
	// call ties the records of one invocation together: cron sets no
	// Message-ID.
	call := newCallID()
	newLogger := func(tag string) *slog.Logger {
		return slog.New(redactor.Handler(d.NewLogger(tag).Handler())).With("call", call)
	}
	log := newLogger(config.DefaultSyslogTag)
	// A panic value may quote a request URL; logging it through the
	// redactor keeps the token out, which the runtime's own crash report
	// would not.
	defer func() {
		if value := recover(); value != nil {
			log.Error("panic, message not delivered", "panic", value)
			code = exitSoftware
		}
	}()
	msg, err := message.Read(stdin)
	if err != nil {
		log.Error("message not read, giving up", "err", err)
		return exitNoInput
	}
	// Headers and body stay out of the log: they may carry anything the
	// calling job printed. The Message-ID, when present, links the records
	// to the message.
	msgAttrs := messageAttrs(msg)
	log = log.With(msgAttrs...)
	log.Info("message received", "size", len(msg.Raw))
	cfg, err := config.Load(d.ConfigFS, d.ConfigPath)
	if err != nil {
		log.Error("configuration rejected, message not delivered", "err", err)
		return exitConfig
	}
	registerSecrets(redactor, cfg)
	if cfg.General.SyslogTag != config.DefaultSyslogTag {
		log = newLogger(cfg.General.SyslogTag).With(msgAttrs...)
	}
	client := *d.HTTP
	client.Timeout = cfg.General.HTTPTimeout.Duration
	targets, err := buildTargets(cfg, d.Hostname, &client)
	if err != nil {
		log.Error("configuration rejected, message not delivered", "err", err)
		return exitConfig
	}
	ctx, cancel := context.WithTimeout(ctx, cfg.General.Deadline.Duration)
	defer cancel()
	payload := backend.Payload{Title: msg.Subject, Text: msg.Body}
	results := delivery.Deliver(ctx, targets, payload)
	for _, r := range results {
		logResult(log, r)
	}
	return delivery.ExitCode(results)
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

// logResult records the outcome of one target: class is temp or perm,
// status the HTTP status when the service answered.
func logResult(log *slog.Logger, r delivery.Result) {
	if r.Status == delivery.OK {
		log.Debug("target delivered", "target", r.TargetID)
		return
	}
	attrs := []any{"target", r.TargetID, "class", r.Status}
	var deliveryErr *backend.Error
	if errors.As(r.Err, &deliveryErr) && deliveryErr.Status != 0 {
		attrs = append(attrs, "status", deliveryErr.Status)
	}
	log.Error("target failed", append(attrs, "err", r.Err)...)
}

// registerSecrets hands every value that may hold a secret to the redactor:
// tokens and URLs, including those read from *_file.
func registerSecrets(redactor *redact.Redactor, cfg *config.Config) {
	for _, name := range cfg.TargetNames() {
		target := cfg.Targets[name]
		redactor.Add(target.Token)
		redactor.AddURL(target.URL)
	}
}

// buildTargets maps configured targets to senders, in name order so that
// logs and delivery order do not depend on map iteration. All targets share
// client, which carries the request timeout.
func buildTargets(cfg *config.Config, hostname string, client *http.Client) ([]delivery.Target, error) {
	var targets []delivery.Target
	for _, name := range cfg.TargetNames() {
		target := cfg.Targets[name]
		switch {
		case target.Type == config.TypeHTTP && target.Preset == config.PresetGenericJSON:
			sender := webhook.New(webhook.Options{URL: target.URL, Hostname: hostname, Client: client})
			targets = append(targets, delivery.Target{ID: name, Sender: sender})
		case target.Type == config.TypeHTTP:
			return nil, fmt.Errorf("target %q: preset %q is not implemented", name, target.Preset)
		default:
			return nil, fmt.Errorf("target %q: type %q is not implemented", name, target.Type)
		}
	}
	return targets, nil
}
