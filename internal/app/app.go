// Package app wires configuration, message, targets and delivery into one
// sendmail invocation. Everything the process takes from the system comes
// in through Deps, so a whole invocation runs in tests without /etc, a
// syslog daemon or a network.
package app

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"

	"github.com/6RUN0/slendmail/internal/backend"
	"github.com/6RUN0/slendmail/internal/backend/webhook"
	"github.com/6RUN0/slendmail/internal/config"
	"github.com/6RUN0/slendmail/internal/delivery"
	"github.com/6RUN0/slendmail/internal/message"
)

// Exit statuses from sysexits.h that Run produces itself.
const (
	exitNoInput = 66
	exitConfig  = 78
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
}

// Run handles one invocation and returns the process exit status. Command
// line arguments are accepted and ignored.
func Run(ctx context.Context, d Deps, _ []string, stdin io.Reader) int {
	log := d.NewLogger(config.DefaultSyslogTag)
	msg, err := message.Read(stdin)
	if err != nil {
		log.Error("message not read, giving up", "err", err)
		return exitNoInput
	}
	cfg, err := config.Load(d.ConfigFS, d.ConfigPath)
	if err != nil {
		log.Error("configuration rejected, message not delivered", "err", err)
		return exitConfig
	}
	if cfg.General.SyslogTag != config.DefaultSyslogTag {
		log = d.NewLogger(cfg.General.SyslogTag)
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
		if r.Status == delivery.OK {
			log.Debug("target delivered", "target", r.TargetID)
			continue
		}
		log.Error("target failed", "target", r.TargetID, "status", r.Status, "err", r.Err)
	}
	return delivery.ExitCode(results)
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
