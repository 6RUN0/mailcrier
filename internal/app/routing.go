package app

import (
	"bytes"
	"context"
	"log/slog"
	"slices"

	"github.com/6RUN0/slendmail/internal/config"
	"github.com/6RUN0/slendmail/internal/delivery"
	"github.com/6RUN0/slendmail/internal/message"
	"github.com/6RUN0/slendmail/internal/route"
	"github.com/6RUN0/slendmail/internal/spool"
)

// verdict is what the rules decide for a message.
type verdict int

const (
	// deliverTo means the message goes to the targets named with it.
	deliverTo verdict = iota
	// suppressed means a [[suppress]] rule matched: the message goes
	// nowhere and counts as handled.
	suppressed
	// noRoute means the rules select no target.
	noRoute
)

// newRouter returns the router of the rules of cfg.
func newRouter(cfg *config.Config) *route.Router {
	rules := make([]route.Rule, len(cfg.Routes))
	for i, r := range cfg.Routes {
		rules[i] = route.Rule{Subject: r.Match.Subject, Sender: r.Match.Sender, Recipient: r.Match.Recipient, Targets: r.Targets, IsContinued: r.Continue}
	}
	suppressions := make([]route.Suppression, len(cfg.Suppressions))
	for i, s := range cfg.Suppressions {
		suppressions[i] = route.Suppression{Subject: s.Match.Subject, Sender: s.Match.Sender}
	}
	return route.New(rules, suppressions)
}

// decide applies the rules to a message: suppression first, then the
// routes of each recipient. It returns the names of the targets, sorted.
// isRepeat marks a decision taken again for a held message, whose
// warnings were logged when it was received: they go to debug, or every
// queue run would repeat them.
func (q *queue) decide(log *slog.Logger, subject string, env message.Envelope, isRepeat bool) ([]string, verdict) {
	if rule, ok := q.router.Suppressed(subject, env.Sender); ok {
		log.Info("message suppressed", "rule", rule)
		return nil, suppressed
	}
	decision := q.router.Targets(subject, env.Sender, route.Addresses(env.Recipients))
	if decision.IsImplicit {
		return q.targetNames(), deliverTo
	}
	log.Debug("message routed", "targets", decision.Names)
	if len(decision.Names) == 0 {
		return nil, noRoute
	}
	if decision.Unrouted > 0 {
		log.Log(context.Background(), repeatLevel(isRepeat), "no route for recipient", "unrouted", decision.Unrouted)
	}
	return decision.Names, deliverTo
}

// repeatLevel is the level of a warning about a message: debug when the
// decision is taken again.
func repeatLevel(isRepeat bool) slog.Level {
	if isRepeat {
		return slog.LevelDebug
	}
	return slog.LevelWarn
}

// targetNames returns the names of all configured targets, sorted.
func (q *queue) targetNames() []string {
	names := make([]string, 0, len(q.targets))
	for name := range q.targets {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// selectTargets returns the targets of names, in name order.
func (q *queue) selectTargets(names []string) []delivery.Target {
	selected := make([]delivery.Target, 0, len(names))
	for _, name := range names {
		if target, ok := q.targets[name]; ok {
			selected = append(selected, target)
		}
	}
	return selected
}

// routeHeld decides the targets of a held entry being released, raw its
// stored message. Without rules every target gets it and the message is
// not read, so that one that cannot be parsed is released all the same.
func (q *queue) routeHeld(log *slog.Logger, e *spool.Entry, raw func() ([]byte, error)) ([]string, verdict, error) {
	if !q.router.HasRules() {
		return q.targetNames(), deliverTo, nil
	}
	data, err := raw()
	if err != nil {
		return nil, 0, err
	}
	msg, _, _, err := readStored(data, e)
	if err != nil {
		return nil, 0, err
	}
	names, v := q.decide(log, msg.Subject, e.Envelope, true)
	return names, v, nil
}

// readStored parses the stored message of entry e.
func readStored(raw []byte, e *spool.Entry) (*message.Message, message.BlindCopies, []string, error) {
	return message.Read(bytes.NewReader(raw), message.ReadOptions{IgnoreDots: true, MaxSize: message.MaxSize, ReceivedAt: e.ReceivedAt})
}
