package sendmail

import (
	"errors"
	"fmt"
	"path"
	"strings"
)

// Mode is what an invocation does.
type Mode uint8

const (
	// Deliver reads a message from stdin and delivers it: -bm, the default.
	Deliver Mode = iota
	// NewAliases does nothing and succeeds: -bi, -I, or the name newaliases.
	NewAliases
	// ListQueue prints the queue: -bp, or the name mailq.
	ListQueue
	// RunQueue runs the queue once: -q, with any interval ignored.
	RunQueue
	// Version prints the version: --version.
	Version
	// Help prints a usage summary: --help.
	Help
	// Probe sends a test message to every target: --probe.
	Probe
	// CheckConfig checks the configuration: --check-config.
	CheckConfig
	// Status prints the queue counters: --status.
	Status
)

// Long options of slendmail itself. They are recognized only where an
// option may start, before "--", so the value of -f or -F never becomes
// one.
const (
	OptionConfig      = "--config"
	OptionVersion     = "--version"
	OptionHelp        = "--help"
	OptionProbe       = "--probe"
	OptionCheckConfig = "--check-config"
	OptionStatus      = "--status"
	// MarkerEnvConfig is put first on the command line by the re-exec that
	// drops SLENDMAIL_CONFIG from the environment, so that the new process
	// can warn about the variable it no longer sees.
	MarkerEnvConfig = "--ignored-env-config"
)

// longModes maps the long options that select a mode.
var longModes = map[string]Mode{
	OptionVersion:     Version,
	OptionHelp:        Help,
	OptionProbe:       Probe,
	OptionCheckConfig: CheckConfig,
	OptionStatus:      Status,
}

// Invocation is what a command line asks for.
type Invocation struct {
	// Mode is the selected mode; the last mode option wins.
	Mode Mode
	// Sender is the envelope sender from -f or -r; HasSender tells an
	// empty sender ("-f ''" or "-f '<>'") from none given.
	Sender    string
	HasSender bool
	// FullName is the sender's full name from -F.
	FullName string
	// ExtractRecipients is -t: the To, Cc and Bcc headers add recipients.
	ExtractRecipients bool
	// IgnoreDots is -i or -oi: a line with a single dot is ordinary text.
	IgnoreDots bool
	// Recipients are the addresses on the command line, split at commas.
	Recipients []string
	// ConfigPath is the value of --config; empty when absent.
	ConfigPath string
	// HasEnvConfigMarker records MarkerEnvConfig.
	HasEnvConfigMarker bool
}

// Warning is an option Parse accepted without acting on it as the caller
// may expect. Message is a constant text, Option the flag as written,
// without its value. The last warning of a long list is WarningSuppressed
// with Count, the number of warnings left out, and no Option.
type Warning struct {
	Message string
	Option  string
	Count   int
}

// Warning messages.
const (
	WarningIgnored      = "option ignored"
	WarningUnknown      = "unknown option"
	WarningMissingValue = "option value missing"
	WarningSuppressed   = "warnings suppressed"
)

// maxWarnings bounds the warnings of one command line: each becomes a log
// record, and a caller could pass thousands of unknown flags.
const maxWarnings = 16

// argKind tells how a short flag takes its value.
type argKind uint8

const (
	// argNone: the flag has no value; the next letter of a group is
	// another flag.
	argNone argKind = iota
	// argRequired: the rest of the group, or else the next argument, is
	// the value.
	argRequired
	// argOptional: the rest of the group, if any, is the value; the next
	// argument never is.
	argOptional
	// argObsolete: the rest of the group, or else the next argument unless
	// it starts with a dash, is the value. sendmail 8 reads a bare -d or
	// -e this way.
	argObsolete
)

// shortFlags lists the short flags sendmail callers use and how each takes
// its value. A letter missing here is an unknown flag without a value.
var shortFlags = map[byte]argKind{
	'f': argRequired, 'r': argRequired, 'F': argRequired,
	'B': argRequired, 'C': argRequired, 'd': argObsolete, 'h': argRequired,
	'L': argRequired, 'N': argRequired, 'O': argRequired, 'R': argRequired,
	'V': argRequired, 'X': argRequired, 'p': argRequired, 'A': argRequired,
	'o': argRequired, 'b': argRequired, 'e': argObsolete,
	'q': argOptional,
	't': argNone, 'i': argNone, 'v': argNone, 'm': argNone, 'n': argNone,
	'U': argNone, 'G': argNone, 'I': argNone,
}

// Parse reads the command line of one invocation: argv0 is argv[0], args
// the arguments after it.
//
// Flags follow getopt: a value is glued (-froot) or the next argument
// (-f root), flags without a value group (-ti), and "--" ends the options.
// Options and recipients may come in any order before "--". An unknown
// flag or long option yields a warning and is otherwise ignored: a mailer
// that refuses an unfamiliar flag loses the message of a cron job.
//
// A non-nil error is a usage error: -f or -r without a value, a line
// break in the sender, the full name or a recipient, the SMTP mode -bs, or
// --config without a value. Its text quotes no address.
func Parse(argv0 string, args []string) (Invocation, []Warning, error) {
	p := parser{args: args}
	switch path.Base(argv0) {
	case "newaliases":
		p.inv.Mode = NewAliases
	case "mailq":
		p.inv.Mode = ListQueue
	}
	if len(p.args) > 0 && p.args[0] == MarkerEnvConfig {
		p.inv.HasEnvConfigMarker = true
		p.args = p.args[1:]
	}
	for p.next < len(p.args) {
		arg := p.args[p.next]
		p.next++
		var err error
		switch {
		case arg == "--":
			for _, rest := range p.args[p.next:] {
				err = errors.Join(err, p.addRecipients(rest))
			}
			p.next = len(p.args)
		case strings.HasPrefix(arg, "--"):
			err = p.parseLong(arg)
		case len(arg) > 1 && arg[0] == '-':
			err = p.parseGroup(arg[1:])
		default:
			err = p.addRecipients(arg)
		}
		if err != nil {
			return Invocation{}, nil, err
		}
	}
	if p.suppressed > 0 {
		p.warnings = append(p.warnings, Warning{Message: WarningSuppressed, Count: p.suppressed})
	}
	return p.inv, p.warnings, nil
}

// parser holds the state of one Parse call.
type parser struct {
	args     []string
	next     int
	inv      Invocation
	warnings []Warning
	// suppressed counts the warnings over maxWarnings.
	suppressed int
}

func (p *parser) warn(message, option string) {
	if len(p.warnings) == maxWarnings {
		p.suppressed++
		return
	}
	p.warnings = append(p.warnings, Warning{Message: message, Option: option})
}

// parseLong handles "--name" and "--name=value".
func (p *parser) parseLong(arg string) error {
	name, value, hasValue := strings.Cut(arg, "=")
	if name == OptionConfig {
		if !hasValue {
			if p.next == len(p.args) {
				return fmt.Errorf("option %s requires a value", OptionConfig)
			}
			value = p.args[p.next]
			p.next++
		}
		if value == "" {
			return fmt.Errorf("option %s requires a value", OptionConfig)
		}
		p.inv.ConfigPath = value
		return nil
	}
	if mode, ok := longModes[name]; ok && !hasValue {
		p.inv.Mode = mode
		return nil
	}
	p.warn(WarningUnknown, name)
	return nil
}

// parseGroup handles the letters after one dash.
func (p *parser) parseGroup(group string) error {
	for i := 0; i < len(group); i++ {
		letter := group[i]
		option := "-" + string(letter)
		kind, known := shortFlags[letter]
		if !known {
			// One warning per group: a single argument can hold
			// thousands of letters.
			p.warn(WarningUnknown, option)
			for i+1 < len(group) && !isKnownFlag(group[i+1]) {
				i++
			}
			continue
		}
		if kind == argNone {
			p.applyFlag(letter)
			continue
		}
		value := group[i+1:]
		isNextValue := p.next < len(p.args) && !strings.HasPrefix(p.args[p.next], "-")
		if value == "" && kind == argObsolete && isNextValue {
			value = p.args[p.next]
			p.next++
		}
		if value == "" && kind == argRequired {
			if p.next == len(p.args) {
				if letter == 'f' || letter == 'r' {
					return fmt.Errorf("option %s requires a value", option)
				}
				p.warn(WarningMissingValue, option)
				return nil
			}
			value = p.args[p.next]
			p.next++
		}
		return p.applyValue(letter, value)
	}
	return nil
}

// applyFlag acts on a flag without a value.
func (p *parser) applyFlag(letter byte) {
	switch letter {
	case 't':
		p.inv.ExtractRecipients = true
	case 'i':
		p.inv.IgnoreDots = true
	case 'I':
		p.inv.Mode = NewAliases
	}
}

// applyValue acts on a flag with its value, which is empty for an absent
// optional value.
func (p *parser) applyValue(letter byte, value string) error {
	switch letter {
	case 'f', 'r':
		if strings.ContainsAny(value, "\r\n") {
			return errors.New("line break in the sender address")
		}
		if len(value) >= 2 && value[0] == '<' && value[len(value)-1] == '>' {
			value = value[1 : len(value)-1]
		}
		p.inv.Sender, p.inv.HasSender = value, true
	case 'F':
		// The full name ends up in a From header, where a line break
		// would start a header of the caller's choice.
		if strings.ContainsAny(value, "\r\n") {
			return errors.New("line break in the full name")
		}
		p.inv.FullName = value
	case 'o':
		if value == "i" {
			p.inv.IgnoreDots = true
		}
	case 'b':
		return p.applyMode(value)
	case 'q':
		p.inv.Mode = RunQueue
	case 'C':
		p.warn(WarningIgnored, "-C")
	}
	return nil
}

// applyMode handles -b with its value. As in sendmail 8 and Postfix only
// the first letter counts: -bsx is -bs.
func (p *parser) applyMode(value string) error {
	switch firstRune(value) {
	case "m":
		p.inv.Mode = Deliver
	case "i":
		p.inv.Mode = NewAliases
	case "p":
		p.inv.Mode = ListQueue
	case "s":
		return errors.New("option -bs: the SMTP mode is not supported")
	default:
		p.warn(WarningIgnored, "-b"+firstRune(value))
	}
	return nil
}

// firstRune returns the first character of s, so that a warning names the
// mode letter without the rest of a long value.
func firstRune(s string) string {
	for _, r := range s {
		return string(r)
	}
	return ""
}

// addRecipients splits one argument at commas: Debian cron passes
// "MAILTO=a@x, b@y" as the arguments "a@x," and "b@y", fail2ban as one.
func (p *parser) addRecipients(arg string) error {
	if strings.ContainsAny(arg, "\r\n") {
		return errors.New("line break in a recipient address")
	}
	for _, address := range strings.Split(arg, ",") {
		if address = strings.TrimSpace(address); address != "" {
			p.inv.Recipients = append(p.inv.Recipients, address)
		}
	}
	return nil
}

// isKnownFlag reports whether letter is a short flag of shortFlags.
func isKnownFlag(letter byte) bool {
	_, known := shortFlags[letter]
	return known
}
