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
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	"github.com/6RUN0/slendmail/internal/backend"
	"github.com/6RUN0/slendmail/internal/backend/webhook"
	"github.com/6RUN0/slendmail/internal/config"
	"github.com/6RUN0/slendmail/internal/delivery"
	"github.com/6RUN0/slendmail/internal/message"
	"github.com/6RUN0/slendmail/internal/redact"
	"github.com/6RUN0/slendmail/internal/render"
	"github.com/6RUN0/slendmail/internal/sendmail"
)

// Exit statuses from sysexits.h that Run produces itself.
const (
	exitOK       = 0
	exitUsage    = 64
	exitNoInput  = 66
	exitSoftware = 70
	exitNoPerm   = 77
	exitConfig   = 78
)

// envConfig names another configuration file, like --config.
const envConfig = "SLENDMAIL_CONFIG"

// Deps are the parts of the environment an invocation uses.
type Deps struct {
	// NewLogger returns the logger for a syslog tag. Run calls it once with
	// the default tag and again when the configuration sets another one.
	NewLogger func(tag string) *slog.Logger
	// ConfigFS is the file system root, "/" in a real invocation; ConfigPath
	// is the default configuration file relative to it. An absolute path
	// from --config or SLENDMAIL_CONFIG and the secret files named in the
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

	// deliver sends p to the targets; nil means delivery.Deliver. Tests
	// set it to observe the envelope and payload and to choose outcomes.
	deliver func(ctx context.Context, targets []delivery.Target, env message.Envelope, p backend.Payload) []delivery.Result
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
	// A panic value may quote a request URL; logging it through the
	// redactor keeps the token out, which the runtime's own crash report
	// would not.
	defer func() {
		if value := recover(); value != nil {
			log.Error("panic, message not delivered", "panic", value)
			code = exitSoftware
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
		_, _ = fmt.Fprintf(d.Stderr, "slendmail: %v\n", err)
		log.Error("command line rejected", "err", err)
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
	if inv.Mode != sendmail.Deliver {
		return runMode(d, log, inv.Mode)
	}
	configPath, err := selectConfigPath(d, inv, log)
	if err != nil {
		log.Error("configuration rejected, message not delivered", "err", err)
		return exitConfig
	}
	msg, bcc, readWarnings, err := message.Read(stdin, message.ReadOptions{IgnoreDots: inv.IgnoreDots, MaxSize: message.MaxSize, ReceivedAt: d.Now()})
	if err != nil {
		log.Error("message not read, giving up", "err", err)
		return exitNoInput
	}
	env := inv.Envelope(msg, bcc, func() string { return defaultSender(d) })
	// Headers, body and addresses stay out of the log: they may carry
	// anything the calling job printed. The Message-ID, when present,
	// links the records to the message.
	msgAttrs := messageAttrs(msg)
	log = log.With(msgAttrs...)
	for _, w := range readWarnings {
		log.Warn(w)
	}
	log.Info("message received", "size", msg.Size, "recipients", len(env.Recipients))
	cfg, err := config.Load(d.ConfigFS, configPath)
	if err != nil {
		log.Error("configuration rejected, message not delivered", "err", err)
		return exitConfig
	}
	registerSecrets(redactor, cfg)
	if cfg.General.SyslogTag != config.DefaultSyslogTag {
		log = newLogger(cfg.General.SyslogTag).With(msgAttrs...)
	}
	client.Timeout = cfg.General.HTTPTimeout.Duration
	targets, err := buildTargets(cfg, d.Hostname, &client)
	if err != nil {
		log.Error("configuration rejected, message not delivered", "err", err)
		return exitConfig
	}
	ctx, cancel := context.WithTimeout(ctx, cfg.General.Deadline.Duration)
	defer cancel()
	deliver := d.deliver
	if deliver == nil {
		deliver = func(ctx context.Context, targets []delivery.Target, _ message.Envelope, p backend.Payload) []delivery.Result {
			return delivery.Deliver(ctx, targets, p)
		}
	}
	results := deliver(ctx, targets, env, buildPayload(msg, notices(cfg.Strings)))
	for _, r := range results {
		logResult(log, r)
	}
	logOutcome(log, results)
	return delivery.ExitCode(results)
}

// buildPayload returns the text of msg for the targets. A body without
// visible text, as at -m and logrotate send it, becomes the EmptyBody
// notice: services reject an empty message.
func buildPayload(msg *message.Message, strs render.Strings) backend.Payload {
	text := msg.Body
	if strings.TrimSpace(text) == "" {
		text = strs.EmptyBody
	}
	return backend.Payload{Title: msg.Subject, Text: text}
}

// notices returns the built-in notices with the configured ones in place.
func notices(configured config.Strings) render.Strings {
	strs := render.DefaultStrings()
	for _, notice := range []struct {
		value string
		dst   *string
	}{
		{configured.NoSubject, &strs.NoSubject}, {configured.EmptyBody, &strs.EmptyBody}, {configured.Truncated, &strs.Truncated},
		{configured.MoreAttachments, &strs.MoreAttachments},
	} {
		if notice.value != "" {
			*notice.dst = notice.value
		}
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

// logOutcome adds the records for the message as a whole. Without a spool
// a temporary failure loses the message for that target.
func logOutcome(log *slog.Logger, results []delivery.Result) {
	var delivered, temporary []delivery.Result
	for _, r := range results {
		switch r.Status {
		case delivery.OK:
			delivered = append(delivered, r)
		case delivery.Temp:
			temporary = append(temporary, r)
		}
	}
	switch {
	case len(delivered) > 0:
		for _, r := range temporary {
			log.Error("message lost for target", "target", r.TargetID)
		}
	case len(temporary) > 0 && len(temporary) == len(results):
		log.Error("message lost")
	case delivery.ExitCode(results) != exitOK:
		log.Error("message not delivered")
	}
}

// runMode answers the modes that read no message.
func runMode(d Deps, log *slog.Logger, mode sendmail.Mode) int {
	switch mode {
	case sendmail.NewAliases:
		return exitOK
	case sendmail.ListQueue:
		_, _ = fmt.Fprintln(d.Stdout, "queue is empty")
		return exitOK
	case sendmail.RunQueue:
		log.Debug("queue is empty")
		return exitOK
	case sendmail.Version:
		_, _ = fmt.Fprintln(d.Stdout, "slendmail", buildVersion())
		return exitOK
	case sendmail.Help:
		_, _ = io.WriteString(d.Stdout, usage)
		return exitOK
	case sendmail.Probe:
		return refuseMode(d, log, sendmail.OptionProbe, true)
	case sendmail.CheckConfig:
		return refuseMode(d, log, sendmail.OptionCheckConfig, true)
	default:
		return refuseMode(d, log, sendmail.OptionStatus, false)
	}
}

// usage is the text of --help.
const usage = `usage: slendmail [flags] [--] [recipient ...]
       slendmail --version | --help | --config PATH
Reads a message on stdin and delivers it to the targets of
/etc/slendmail.conf. sendmail flags: -t -i -oi -f ADDR -r ADDR -F NAME;
-bi, -I and newaliases do nothing; -bp and mailq list the queue; -q runs it.
Other sendmail flags are accepted and ignored. See slendmail(8).
`

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

// refuseMode answers --probe, --check-config and --status, which do not
// exist yet. The first two read the whole configuration or send to every
// target, so with isRestricted an elevated caller other than root and the
// slendmail user gets 77; everyone else gets a usage error.
func refuseMode(d Deps, log *slog.Logger, mode string, isRestricted bool) int {
	if isRestricted && !d.Credentials.isPrivilegedCaller() {
		_, _ = fmt.Fprintf(d.Stderr, "slendmail: %s: permission denied\n", mode)
		log.Error("mode refused, caller not privileged", "option", mode, "uid", d.Credentials.UID)
		return exitNoPerm
	}
	_, _ = fmt.Fprintf(d.Stderr, "slendmail: %s: not implemented\n", mode)
	log.Error("mode refused, not implemented", "option", mode)
	return exitUsage
}

// selectConfigPath returns the configuration file relative to d.ConfigFS:
// --config wins over SLENDMAIL_CONFIG, which wins over the default. An
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
// temp or perm, status the HTTP status when the service answered.
func logResult(log *slog.Logger, r delivery.Result) {
	switch r.Status {
	case delivery.OK:
		log.Debug("target delivered", "target", r.TargetID)
		return
	case delivery.Suppressed:
		log.Info("target suppressed", "target", r.TargetID)
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
