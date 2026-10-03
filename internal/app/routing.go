package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/6RUN0/mailcrier/internal/backend/telegram"
	"github.com/6RUN0/mailcrier/internal/config"
	"github.com/6RUN0/mailcrier/internal/delivery"
	"github.com/6RUN0/mailcrier/internal/message"
	"github.com/6RUN0/mailcrier/internal/route"
	"github.com/6RUN0/mailcrier/internal/spool"
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

// directChats make the copies of the telegram_direct target that deliver
// to the recipients <chat>@telegram.
type directChats struct {
	// base is the target the copies are made of, and options the options
	// of its sender.
	base    delivery.Target
	options telegram.Options
	// baseChat is the chat of base, normalized; empty when it is no chat
	// a direct address can name.
	baseChat string
	// allowed are the chats a recipient may name, limit the most chats of
	// one message.
	allowed []string
	limit   int
}

// newDirectChats returns the direct chats of cfg, nil without
// telegram_direct. Load has checked that it names a telegram target.
func newDirectChats(cfg *config.Config, targets []delivery.Target, client *http.Client) *directChats {
	name := cfg.General.TelegramDirect
	if name == "" {
		return nil
	}
	base := cfg.Targets[name]
	direct := &directChats{options: telegramOptions(base, client), allowed: cfg.General.TelegramDirectChats, limit: cfg.General.TelegramDirectMax}
	direct.baseChat, _ = route.NormalizeChat(base.ChatID)
	for _, target := range targets {
		if target.ID == name {
			direct.base = target
		}
	}
	return direct
}

// directTarget returns the copy of base, whose sender has options, for the
// chat of a direct recipient. The template, limits and file policy are
// those of base; the topic is not, as it belongs to the chat of base and
// another chat would refuse it.
func directTarget(base delivery.Target, options telegram.Options, chat string) delivery.Target {
	options.ChatID, options.MessageThreadID = chat, 0
	target := base
	target.ID, target.Sender = chat+"@"+route.DirectDomain, telegram.New(options)
	return target
}

// decide applies the rules to a message: suppression first, then the
// direct chats among the recipients, then the routes of the others. It
// returns the names of the targets: those the routes select, sorted, then
// the direct chats. isRepeat marks a decision taken again for a held
// message, whose warnings were logged when it was received: they go to
// debug, or every queue run would repeat them.
func (q *queue) decide(log *slog.Logger, subject string, env message.Envelope, isRepeat bool) ([]string, verdict) {
	if rule, ok := q.router.Suppressed(subject, env.Sender); ok {
		log.Info("message suppressed", "rule", rule)
		return nil, suppressed
	}
	ctx, level := context.Background(), repeatLevel(isRepeat)
	recipients := route.Addresses(env.Recipients)
	var chats []string
	if q.direct != nil {
		split := route.SplitDirect(recipients, q.direct.allowed, q.direct.limit)
		if split.Invalid > 0 {
			log.Log(ctx, level, "direct address invalid", "count", split.Invalid)
		}
		if split.NotAllowed > 0 {
			log.Log(ctx, level, "direct chat not allowed", "count", split.NotAllowed)
		}
		if split.Dropped > 0 {
			log.Log(ctx, level, "direct chats over limit", "dropped", split.Dropped)
		}
		chats, recipients = split.Chats, split.Rest
	}
	// A message only to direct chats leaves the routes no recipient.
	var names []string
	if len(chats) == 0 || len(recipients) > 0 {
		decision := q.router.Targets(subject, env.Sender, recipients)
		names = decision.Names
		if decision.IsImplicit {
			names = q.targetNames()
		} else {
			log.Debug("message routed", "targets", decision.Names)
		}
		if decision.Unrouted > 0 && (len(names) > 0 || len(chats) > 0) {
			log.Log(ctx, level, "no route for recipient", "unrouted", decision.Unrouted)
		}
	}
	for _, chat := range chats {
		if chat == q.direct.baseChat && slices.Contains(names, q.direct.base.ID) {
			log.Debug("direct chat is the chat of its target", "target", q.direct.base.ID)
			continue
		}
		names = append(names, chat+"@"+route.DirectDomain)
	}
	if len(names) == 0 {
		return nil, noRoute
	}
	return names, deliverTo
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

// selectTargets returns the targets of names, in their order.
func (q *queue) selectTargets(names []string) []delivery.Target {
	selected := make([]delivery.Target, 0, len(names))
	for _, name := range names {
		if target, ok := q.target(name); ok {
			selected = append(selected, target)
		}
	}
	return selected
}

// target returns the target of name: a configured one, or the copy for a
// direct chat, made again on every call from the configuration of the
// moment. false when the target or the chat is gone from it.
func (q *queue) target(name string) (delivery.Target, bool) {
	if target, ok := q.targets[name]; ok {
		return target, true
	}
	chat, isDirect := strings.CutSuffix(name, "@"+route.DirectDomain)
	if !isDirect || q.direct == nil || !slices.Contains(q.direct.allowed, chat) {
		return delivery.Target{}, false
	}
	return directTarget(q.direct.base, q.direct.options, chat), true
}

// errHeldRead wraps an error of reading a held message from the spool, as
// opposed to parsing it: only the first makes -q exit 74.
var errHeldRead = errors.New("held message not read")

// routeHeld decides the targets of a held entry being released, raw its
// stored message. Without rules and direct chats every target gets it and
// the message is not read, so that one that cannot be read is released all
// the same. An error of raw wraps errHeldRead.
func (q *queue) routeHeld(log *slog.Logger, e *spool.Entry, raw func() ([]byte, error)) ([]string, verdict, error) {
	if !q.router.HasRules() && q.direct == nil {
		return q.targetNames(), deliverTo, nil
	}
	data, err := raw()
	if err != nil {
		return nil, 0, fmt.Errorf("%w: %w", errHeldRead, err)
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
