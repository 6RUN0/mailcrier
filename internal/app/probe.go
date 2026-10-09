package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/6RUN0/mailcrier/internal/backend"
	"github.com/6RUN0/mailcrier/internal/config"
	"github.com/6RUN0/mailcrier/internal/delivery"
	"github.com/6RUN0/mailcrier/internal/redact"
	"github.com/6RUN0/mailcrier/internal/sendmail"
)

// runProbe answers --probe: it sends the sample message to the targets of
// the configuration, or to those inv names, and prints one logfmt line per
// target. Routes, suppression and direct chats do not apply; hooks run.
// The spool is not touched: a failed target is not queued, and the queue
// is not run. It exits with probeExitCode.
func runProbe(ctx context.Context, d Deps, log *slog.Logger, newLogger func(tag string) *slog.Logger, redactor *redact.Redactor, client *http.Client, inv sendmail.Invocation) int {
	configPath, err := selectConfigPath(d, inv, log)
	if err != nil {
		_, _ = fmt.Fprintf(d.Stderr, "error: %v\n", err)
		log.Error("configuration rejected", "err", err)
		return exitConfig
	}
	cfg, targets, log, err := loadTargets(d, log, newLogger, redactor, client, configPath, config.Load)
	if err != nil {
		var cfgErr *config.Error
		text := err.Error()
		if !errors.As(err, &cfgErr) {
			text = "/" + configPath + ": " + text
		}
		_, _ = fmt.Fprintln(d.Stderr, redactor.String("error: "+text))
		log.Error("configuration rejected", "err", err)
		return exitConfig
	}
	selected, unknown := selectProbeTargets(targets, inv.Recipients)
	if unknown != "" {
		_, _ = fmt.Fprintf(d.Stderr, "mailcrier: %s: no target %q\n", sendmail.OptionProbe, unknown)
		log.Error("probe target unknown", "target", unknown)
		return exitUsage
	}
	msg, env, data, err := readSample(d, notices(cfg.Strings))
	if err != nil {
		log.Error("sample message not rendered", "err", err)
		return exitSoftware
	}
	sendCtx, cancel := context.WithTimeout(ctx, cfg.General.Deadline.Duration)
	defer cancel()
	var results []delivery.Result
	if d.deliver == nil {
		results, _ = delivery.DeliverEach(sendCtx, selected, data, msg.Attachments, msg.Raw, nil)
	} else {
		results = d.deliver(sendCtx, selected, env, data, msg.Attachments)
	}
	for _, r := range results {
		logResult(log, redactor, r, false)
		_, _ = fmt.Fprintln(d.Stdout, probeLine(redactor, r))
	}
	log.Info("probe sent", "targets", len(selected))
	return probeExitCode(results)
}

// selectProbeTargets returns the targets that names lists, all of them
// when names is empty, in the order of targets; a name given twice selects
// its target once. unknown is the first name of no target.
func selectProbeTargets(targets []delivery.Target, names []string) (selected []delivery.Target, unknown string) {
	if len(names) == 0 {
		return targets, ""
	}
	for _, name := range names {
		if !slices.ContainsFunc(targets, func(t delivery.Target) bool { return t.ID == name }) {
			return nil, name
		}
	}
	for _, target := range targets {
		if slices.Contains(names, target.ID) {
			selected = append(selected, target)
		}
	}
	return selected, ""
}

// probeLine describes the result of one target as a logfmt line, such as
// target=mm class=temp status=503 err="...": the HTTP status when the
// service answered, template=fallback when the built-in template stood in
// for the configured one, and the error with its secrets masked.
func probeLine(redactor *redact.Redactor, r delivery.Result) string {
	fields := []string{"target=" + r.TargetID, "class=" + r.Status.String()}
	var deliveryErr *backend.Error
	if errors.As(r.Err, &deliveryErr) && deliveryErr.Status != 0 {
		fields = append(fields, fmt.Sprintf("status=%d", deliveryErr.Status))
	}
	if r.TemplateErr != nil {
		fields = append(fields, "template=fallback")
	}
	if r.Err != nil {
		fields = append(fields, fmt.Sprintf("err=%q", redactor.String(r.Err.Error())))
	}
	return strings.Join(fields, " ")
}

// probeExitCode is the exit status of --probe: 0 when every target took
// the sample, 69 when one rejected it, 75 when one failed temporarily and
// none rejected it. A call with a message never exits 75, as its spool
// retries; the probe has no spool and reports the failure as it is.
func probeExitCode(results []delivery.Result) int {
	code := exitOK
	for _, r := range results {
		switch r.Status {
		case delivery.Perm:
			return exitUnavailable
		case delivery.Temp:
			code = exitTempFail
		}
	}
	return code
}
