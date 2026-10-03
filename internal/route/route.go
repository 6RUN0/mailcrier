package route

import (
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/6RUN0/mailcrier/internal/message"
)

// Rule is one route: the message goes to Targets when every condition
// that is set matches. A nil condition is not set.
type Rule struct {
	// Subject, Sender and Recipient match the subject, the envelope
	// sender and one envelope recipient.
	Subject, Sender, Recipient *regexp.Regexp
	// Targets are the names of the targets the rule selects.
	Targets []string
	// IsContinued lets the rules after this one match too.
	IsContinued bool
}

// Suppression is one suppression rule: a message that matches every
// condition that is set goes nowhere. A nil condition is not set.
type Suppression struct {
	// Subject and Sender match the subject and the envelope sender.
	Subject, Sender *regexp.Regexp
}

// Router selects targets by rules and suppresses messages. Its zero value
// has no rules and suppresses nothing.
type Router struct {
	rules        []Rule
	suppressions []Suppression
}

// New returns a Router of rules, checked in order, and suppressions.
func New(rules []Rule, suppressions []Suppression) *Router {
	return &Router{rules: rules, suppressions: suppressions}
}

// HasRules reports whether the router has a rule or a suppression, that
// is whether its decisions depend on the message.
func (r *Router) HasRules() bool {
	return len(r.rules) > 0 || len(r.suppressions) > 0
}

// Suppressed returns the number, from 1, of the first suppression rule
// that matches subject and sender; ok is false when none does.
func (r *Router) Suppressed(subject, sender string) (rule int, ok bool) {
	for i, s := range r.suppressions {
		if matches(s.Subject, subject) && matches(s.Sender, sender) {
			return i + 1, true
		}
	}
	return 0, false
}

// Decision is the outcome of Targets.
type Decision struct {
	// Names are the selected targets, sorted and unique; empty when no
	// rule matched.
	Names []string
	// IsImplicit means that there are no rules: every target gets the
	// message, and Names is empty.
	IsImplicit bool
	// Unrouted counts the recipients that no rule matched.
	Unrouted int
}

// Targets selects the targets of a message. For each recipient, a value
// of Addresses, the rules are checked from the first: the targets of every
// rule that matches are taken, and the check stops at the first match
// that does not continue. The targets of all recipients are joined. A
// message without recipients is checked once, and a rule with a recipient
// condition does not match it.
func (r *Router) Targets(subject, sender string, recipients []string) Decision {
	if len(r.rules) == 0 {
		return Decision{IsImplicit: true}
	}
	var decision Decision
	selectFor := func(recipient string, hasRecipient bool) bool {
		isMatched := false
		for _, rule := range r.rules {
			if !matches(rule.Subject, subject) || !matches(rule.Sender, sender) ||
				(rule.Recipient != nil && (!hasRecipient || !rule.Recipient.MatchString(recipient))) {
				continue
			}
			isMatched = true
			decision.Names = append(decision.Names, rule.Targets...)
			if !rule.IsContinued {
				break
			}
		}
		return isMatched
	}
	if len(recipients) == 0 {
		selectFor("", false)
	}
	for _, recipient := range recipients {
		if !selectFor(recipient, true) {
			decision.Unrouted++
		}
	}
	slices.Sort(decision.Names)
	decision.Names = slices.Compact(decision.Names)
	return decision
}

// matches reports whether condition, when set, matches value.
func matches(condition *regexp.Regexp, value string) bool {
	return condition == nil || condition.MatchString(value)
}

// Addresses returns the addresses of the envelope recipients, which come
// from the command line as written ("Ops <ops@example.org>") or from
// headers: each value is parsed as an address list, a value without an
// address is left out, one with several gives each, and duplicates are
// dropped.
func Addresses(recipients []string) []string {
	var addrs []string
	for _, recipient := range recipients {
		for _, a := range message.ParseAddressList(recipient) {
			if a.Addr != "" && !slices.Contains(addrs, a.Addr) {
				addrs = append(addrs, a.Addr)
			}
		}
	}
	return addrs
}

// DirectDomain is the domain of the addresses of direct chats, such as
// 1234@telegram.
const DirectDomain = "telegram"

// validUsername matches the @username of a public chat.
var validUsername = regexp.MustCompile(`^@[A-Za-z0-9_]{5,32}$`)

// NormalizeChat returns the chat id as the Bot API takes it, so that
// spellings of one chat compare equal: a numeric id without leading zeros
// or plus sign, an @username in lower case. ok is false for anything else.
func NormalizeChat(chat string) (id string, ok bool) {
	if validUsername.MatchString(chat) {
		return strings.ToLower(chat), true
	}
	if chat == "" || strings.HasPrefix(chat, "+") {
		return "", false
	}
	n, err := strconv.ParseInt(chat, 10, 64)
	if err != nil {
		return "", false
	}
	return strconv.FormatInt(n, 10), true
}

// Direct is the outcome of SplitDirect.
type Direct struct {
	// Chats are the chats to send a copy to, normalized and unique, in the
	// order of the recipients.
	Chats []string
	// Rest are the recipients that are no direct chat, in order; the
	// rules route them.
	Rest []string
	// Invalid counts the addresses of DirectDomain whose local part is no
	// chat id, and NotAllowed those of a chat not in the allowed list;
	// both stay among Rest.
	Invalid, NotAllowed int
	// Dropped counts the allowed chats past the limit, which get no copy.
	Dropped int
}

// SplitDirect separates the addresses of direct chats, chat@telegram with
// the domain in any case, from the other recipients, values of Addresses.
// A chat must be in allowed, which holds normalized ids; at most limit
// chats are taken.
func SplitDirect(recipients, allowed []string, limit int) Direct {
	var direct Direct
	var dropped []string
	for _, recipient := range recipients {
		at := strings.LastIndexByte(recipient, '@')
		if at < 0 || !strings.EqualFold(recipient[at+1:], DirectDomain) {
			direct.Rest = append(direct.Rest, recipient)
			continue
		}
		chat, ok := NormalizeChat(recipient[:at])
		switch {
		case !ok:
			direct.Invalid++
			direct.Rest = append(direct.Rest, recipient)
		case !slices.Contains(allowed, chat):
			direct.NotAllowed++
			direct.Rest = append(direct.Rest, recipient)
		case slices.Contains(direct.Chats, chat), slices.Contains(dropped, chat):
		case len(direct.Chats) >= limit:
			dropped = append(dropped, chat)
		default:
			direct.Chats = append(direct.Chats, chat)
		}
	}
	direct.Dropped = len(dropped)
	return direct
}
