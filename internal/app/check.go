package app

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"

	"github.com/6RUN0/slendmail/internal/backend"
	"github.com/6RUN0/slendmail/internal/config"
	"github.com/6RUN0/slendmail/internal/delivery"
	"github.com/6RUN0/slendmail/internal/redact"
	"github.com/6RUN0/slendmail/internal/sendmail"
)

// modeOptions names the service modes by their option, for messages.
var modeOptions = map[sendmail.Mode]string{sendmail.Probe: sendmail.OptionProbe, sendmail.CheckConfig: sendmail.OptionCheckConfig}

// admitServiceMode decides whether a service mode, --probe or
// --check-config, may run, before the configuration is chosen and without
// reading stdin. It refuses with 77 an elevated caller other than root and
// the slendmail user: the modes read the whole configuration or send to
// every target. It refuses with 64 a --config or SLENDMAIL_CONFIG that an
// elevated process ignores, so that the mode never reports on another
// file than the caller named, and arguments of --check-config.
func admitServiceMode(d Deps, log *slog.Logger, inv sendmail.Invocation) (code int, isAdmitted bool) {
	option := modeOptions[inv.Mode]
	if !d.Credentials.isPrivilegedCaller() {
		_, _ = fmt.Fprintf(d.Stderr, "slendmail: %s: permission denied\n", option)
		log.Error("mode refused, caller not privileged", "option", option, "uid", d.Credentials.UID)
		return exitNoPerm, false
	}
	if d.Credentials.isElevated() {
		source := ""
		if value, ok := lookupEnv(d.Environ, envConfig); inv.HasEnvConfigMarker || (ok && value != "") {
			source = envConfig
		}
		if inv.ConfigPath != "" {
			source = sendmail.OptionConfig
		}
		if source != "" {
			_, _ = fmt.Fprintf(d.Stderr, "slendmail: %s is ignored for a setgid-elevated caller\n", source)
			log.Error("configuration override refused", "option", option, "source", source)
			return exitUsage, false
		}
	}
	if inv.Mode == sendmail.CheckConfig && len(inv.Recipients) > 0 {
		_, _ = fmt.Fprintf(d.Stderr, "slendmail: %s takes no arguments\n", option)
		log.Error("command line rejected", "err", option+" takes no arguments")
		return exitUsage, false
	}
	return exitOK, true
}

// runCheckConfig answers --check-config: it loads the configuration and
// builds its targets as a call does, adds the warnings of config.Check and
// renders the sample message for every target without sending it. The
// findings go to stderr, one per line, then a line with their numbers;
// stdout stays empty. It exits 78 on an error, 0 otherwise.
func runCheckConfig(ctx context.Context, d Deps, log *slog.Logger, newLogger func(tag string) *slog.Logger, redactor *redact.Redactor, client *http.Client, inv sendmail.Invocation) int {
	configPath, err := selectConfigPath(d, inv, log)
	if err != nil {
		_, _ = fmt.Fprintf(d.Stderr, "error: %v\n", err)
		log.Info("configuration checked", "errors", 1, "warnings", 0)
		return exitConfig
	}
	// The setgid bit gives the group through which every user other than
	// root reads the files; root is no exception, as the kernel applies the
	// bit to it as well.
	group := -1
	if c := d.Credentials; c.EGID != c.GID {
		group = c.EGID
	}
	var warnings []config.Warning
	check := func(fsys fs.FS, path string) (*config.Config, error) {
		cfg, found, err := config.Check(fsys, path, group)
		warnings = found
		return cfg, err
	}
	cfg, targets, log, err := loadTargets(d, log, newLogger, redactor, client, configPath, check)
	var lines []string
	errorCount := 0
	if err != nil {
		errorCount = 1
		var cfgErr *config.Error
		if errors.As(err, &cfgErr) {
			lines = append(lines, "error: "+cfgErr.Error())
		} else {
			lines = append(lines, "error: /"+configPath+": "+err.Error())
		}
	}
	for _, w := range warnings {
		lines = append(lines, "warning: "+w.String())
	}
	if err == nil {
		findings, renderErr := renderSample(ctx, d, cfg, targets)
		if renderErr != nil {
			log.Error("sample message not rendered", "err", renderErr)
			return exitSoftware
		}
		for _, finding := range findings {
			lines = append(lines, "warning: /"+configPath+": "+finding)
		}
	}
	warningCount := len(lines) - errorCount
	lines = append(lines, fmt.Sprintf("/%s: %d %s, %d %s", configPath, errorCount, plural(errorCount, "error"), warningCount, plural(warningCount, "warning")))
	for _, line := range lines {
		_, _ = fmt.Fprintln(d.Stderr, redactor.String(line))
	}
	log.Info("configuration checked", "errors", errorCount, "warnings", warningCount)
	if err != nil {
		return exitConfig
	}
	return exitOK
}

// plural returns noun, with an s unless count is 1.
func plural(count int, noun string) string {
	if count == 1 {
		return noun
	}
	return noun + "s"
}

// renderSample renders the sample message for every target as a delivery
// does, with the senders replaced by ones that send nothing: no request
// leaves the host and no hook runs. It returns what a target would not get
// as configured, "target "name": what", in the order of targets.
func renderSample(ctx context.Context, d Deps, cfg *config.Config, targets []delivery.Target) ([]string, error) {
	_, _, data, err := readSample(d, notices(cfg.Strings))
	if err != nil {
		return nil, err
	}
	dry := make([]delivery.Target, len(targets))
	for i, target := range targets {
		dry[i] = target
		dry[i].Sender = dryRunSender{caps: target.Sender.Caps()}
	}
	renderCtx, cancel := context.WithTimeout(ctx, cfg.General.Deadline.Duration)
	defer cancel()
	var findings []string
	for _, r := range delivery.Deliver(renderCtx, dry, data, nil) {
		var finding string
		switch {
		case r.TemplateErr != nil && r.Status == delivery.OK:
			finding = "template fails on the sample message, the built-in one is used: " + r.TemplateErr.Error()
		case r.RequestErr != nil:
			finding = "request template fails on the sample message, the target gets nothing: " + r.RequestErr.Error()
		case r.Status == delivery.Perm:
			finding = "the sample message does not render, the target gets nothing: " + r.Err.Error()
		default:
			continue
		}
		findings = append(findings, fmt.Sprintf("target %q: %s", r.TargetID, finding))
	}
	return findings, nil
}

// dryRunSender stands in for the sender of a target in renderSample: it
// reports the limits of the real one and accepts every payload.
type dryRunSender struct {
	caps backend.Caps
}

// Caps returns the limits of the sender it stands in for.
func (s dryRunSender) Caps() backend.Caps {
	return s.caps
}

// Send accepts the payload without sending it.
func (dryRunSender) Send(context.Context, backend.Payload) error {
	return nil
}
