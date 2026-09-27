// Package config loads and validates the slendmail TOML configuration.
//
// Parsing is strict on every run: an unknown key, a key that does not
// belong to the target type, or a failed validation rejects the whole file.
// The package knows nothing about target implementations; the caller maps
// Target values to senders.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// DefaultSyslogTag is the syslog tag used when [general] does not set one.
const DefaultSyslogTag = "slendmail"

// Defaults of the [general] time limits. A request timeout below the
// deadline leaves room for a second target after the first one hangs. The
// deadline covers the delivery to all targets, where a call can hang on the
// network; reading stdin and the configuration are not under it.
const (
	DefaultHTTPTimeout = 15 * time.Second
	DefaultDeadline    = 30 * time.Second
)

// Target types.
const (
	TypeTelegram = "telegram"
	TypeDiscord  = "discord"
	TypeSlack    = "slack"
	TypeNtfy     = "ntfy"
	TypeHTTP     = "http"
	TypeExec     = "exec"
	TypeShoutrrr = "shoutrrr"
)

// Built-in payload presets of the http target.
const (
	PresetMattermost   = "mattermost"
	PresetSlackWebhook = "slack-webhook"
	PresetGenericJSON  = "generic-json"
)

// Policies for text longer than the target accepts.
const (
	OnLongFile       = "file"
	OnLongTruncate   = "truncate"
	OnLongBlockquote = "blockquote"
)

// Config is the whole configuration file.
type Config struct {
	// General holds process-wide settings.
	General General `toml:"general"`
	// Strings overrides the notices written in place of missing content;
	// an empty field keeps the built-in English text.
	Strings Strings `toml:"strings"`
	// Targets maps the target name, the key of [target.<name>], to its
	// settings. TOML rejects a table defined twice, so names are unique.
	Targets map[string]Target `toml:"target"`
}

// General is the [general] table.
type General struct {
	// SyslogTag is the tag of every syslog record.
	SyslogTag string `toml:"syslog_tag"`
	// HTTPTimeout bounds one HTTP request, from dialing to the end of the
	// response body.
	HTTPTimeout Duration `toml:"http_timeout"`
	// Deadline bounds the delivery of the message to all targets.
	Deadline Duration `toml:"deadline"`
}

// Strings is the [strings] table.
type Strings struct {
	// NoSubject stands in for a missing subject.
	NoSubject string `toml:"no_subject"`
	// EmptyBody stands in for a body without visible text.
	EmptyBody string `toml:"empty_body"`
	// Truncated ends a body cut to the length limit of a target.
	Truncated string `toml:"truncated"`
	// MoreAttachments follows a list of attachments cut to the length
	// limit of a target; its one %d is the number left out.
	MoreAttachments string `toml:"more_attachments"`
	// NotSent follows an attachment listed in the text but not sent,
	// because it exceeds a file limit of the target.
	NotSent string `toml:"not_sent"`
}

// Duration is a time span written as a Go duration string, such as "15s".
type Duration struct {
	time.Duration
}

// UnmarshalText parses a duration string. The error does not quote the
// value, as no configuration error does.
func (d *Duration) UnmarshalText(text []byte) error {
	parsed, err := time.ParseDuration(string(text))
	if err != nil {
		return errors.New("invalid duration, want a Go duration such as \"15s\"")
	}
	d.Duration = parsed
	return nil
}

// Target is one [target.<name>] table. It is the union of the keys of all
// target types; Load rejects a key that the type does not use.
type Target struct {
	// Type selects the target implementation.
	Type string `toml:"type"`
	// Token is the API token. After Load it also holds the content of
	// TokenFile.
	Token string `toml:"token"`
	// TokenFile is an absolute path to a file holding the token.
	TokenFile string `toml:"token_file"`
	// URL is the endpoint. After Load it also holds the content of URLFile.
	URL string `toml:"url"`
	// URLFile is an absolute path to a file holding the URL, for URLs that
	// embed a secret.
	URLFile string `toml:"url_file"`
	// ChatID is the Telegram chat.
	ChatID string `toml:"chat_id"`
	// MessageThreadID is the Telegram forum topic.
	MessageThreadID int64 `toml:"message_thread_id"`
	// Channel is the Slack channel.
	Channel string `toml:"channel"`
	// Preset selects the built-in payload of the http target.
	Preset string `toml:"preset"`
	// OnLong is the policy for text longer than the target accepts.
	OnLong string `toml:"on_long"`
	// Argv is the command line of the exec target.
	Argv []string `toml:"argv"`
}

// allowedKeys lists, per target type, the keys it uses besides "type". The
// http target lists only what its implementation honours, so that a key it
// would ignore rejects the file instead.
var allowedKeys = map[string][]string{
	TypeTelegram: {"token", "token_file", "chat_id", "message_thread_id", "on_long"},
	TypeDiscord:  {"url", "url_file", "on_long"},
	TypeSlack:    {"token", "token_file", "channel", "on_long"},
	TypeNtfy:     {"url", "url_file", "on_long"},
	TypeHTTP:     {"url", "url_file", "preset"},
	TypeExec:     {"argv"},
	TypeShoutrrr: {"url", "url_file"},
}

var (
	presets        = []string{PresetMattermost, PresetSlackWebhook, PresetGenericJSON}
	onLongPolicies = []string{OnLongFile, OnLongTruncate, OnLongBlockquote}
	validName      = regexp.MustCompile(`^[a-z0-9-]+$`)
)

// Error is a configuration error. Its text never quotes configuration
// values, because the file holds secrets and the text goes to syslog.
type Error struct {
	// Path is the configuration file.
	Path string
	// Line and Column locate the offending key; 0 when unknown.
	Line, Column int
	// Msg describes the problem.
	Msg string
}

// Error returns "path:line:column: message", without the position when it
// is unknown.
func (e *Error) Error() string {
	if e.Line == 0 {
		return fmt.Sprintf("%s: %s", e.Path, e.Msg)
	}
	return fmt.Sprintf("%s:%d:%d: %s", e.Path, e.Line, e.Column, e.Msg)
}

// Load reads the configuration at path in fsys, decodes it strictly,
// validates it and replaces every *_file reference with the file content.
// Secret files are read from fsys as well, with the leading slash removed
// from their absolute path. Every returned error is *Error.
func Load(fsys fs.FS, path string) (*Config, error) {
	doc, err := fs.ReadFile(fsys, path)
	if err != nil {
		return nil, &Error{Path: path, Msg: err.Error()}
	}
	var cfg Config
	dec := toml.NewDecoder(bytes.NewReader(doc)).DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return nil, decodeError(path, err)
	}
	keys, err := indexKeys(doc)
	if err != nil {
		return nil, &Error{Path: path, Msg: err.Error()}
	}
	if err := validate(&cfg, keys); err != nil {
		err.Path = path
		return nil, err
	}
	if err := readSecretFiles(fsys, &cfg, keys); err != nil {
		err.Path = path
		return nil, err
	}
	if err := validateSecrets(&cfg, keys); err != nil {
		err.Path = path
		return nil, err
	}
	if cfg.General.SyslogTag == "" {
		cfg.General.SyslogTag = DefaultSyslogTag
	}
	if !keys.has("general", "http_timeout") {
		cfg.General.HTTPTimeout.Duration = DefaultHTTPTimeout
	}
	if !keys.has("general", "deadline") {
		cfg.General.Deadline.Duration = DefaultDeadline
	}
	return &cfg, nil
}

// decodeError keeps only the position and the key of a go-toml error:
// DecodeError.String quotes the surrounding document, which may hold a
// token.
func decodeError(path string, err error) *Error {
	var strict *toml.StrictMissingError
	if errors.As(err, &strict) && len(strict.Errors) > 0 {
		first := strict.Errors[0]
		line, column := first.Position()
		return &Error{Path: path, Line: line, Column: column, Msg: fmt.Sprintf("unknown key %q", strings.Join(first.Key(), "."))}
	}
	var decode *toml.DecodeError
	if errors.As(err, &decode) {
		line, column := decode.Position()
		return &Error{Path: path, Line: line, Column: column, Msg: decode.Error()}
	}
	return &Error{Path: path, Msg: err.Error()}
}

func validate(cfg *Config, keys keyIndex) *Error {
	for _, limit := range []struct {
		key   string
		value time.Duration
	}{{"http_timeout", cfg.General.HTTPTimeout.Duration}, {"deadline", cfg.General.Deadline.Duration}} {
		if keys.has("general", limit.key) && limit.value <= 0 {
			pos := keys.position("general", limit.key)
			return &Error{Line: pos.Line, Column: pos.Column, Msg: fmt.Sprintf("value of key %q must be positive", limit.key)}
		}
	}
	for _, notice := range []struct{ key, value string }{
		{"no_subject", cfg.Strings.NoSubject}, {"empty_body", cfg.Strings.EmptyBody}, {"truncated", cfg.Strings.Truncated},
		{"more_attachments", cfg.Strings.MoreAttachments}, {"not_sent", cfg.Strings.NotSent},
	} {
		if keys.has("strings", notice.key) && strings.TrimSpace(notice.value) == "" {
			pos := keys.position("strings", notice.key)
			return &Error{Line: pos.Line, Column: pos.Column, Msg: fmt.Sprintf("value of key %q must not be blank", notice.key)}
		}
	}
	if keys.has("strings", "more_attachments") && (strings.Count(cfg.Strings.MoreAttachments, "%d") != 1 || strings.Count(cfg.Strings.MoreAttachments, "%") != 1) {
		pos := keys.position("strings", "more_attachments")
		return &Error{Line: pos.Line, Column: pos.Column, Msg: `value of key "more_attachments" must hold one %d and no other %`}
	}
	if len(cfg.Targets) == 0 {
		return &Error{Msg: "no targets configured"}
	}
	for _, name := range cfg.TargetNames() {
		if err := validateTarget(name, cfg.Targets[name], keys); err != nil {
			return err
		}
	}
	return nil
}

func validateTarget(name string, target Target, keys keyIndex) *Error {
	fail := func(key, format string, args ...any) *Error {
		pos := keys.position("target", name, key)
		return &Error{Line: pos.Line, Column: pos.Column, Msg: fmt.Sprintf("target %q: ", name) + fmt.Sprintf(format, args...)}
	}
	if !validName.MatchString(name) {
		return fail("", "name must match %s", validName)
	}
	allowed, known := allowedKeys[target.Type]
	if !known {
		if target.Type == "" {
			return fail("", "key \"type\" is required")
		}
		return fail("type", "unknown type, want one of %s", strings.Join(sortedTypes(), ", "))
	}
	for _, key := range keys.children("target", name) {
		if key != "type" && !slices.Contains(allowed, key) {
			return fail(key, "key %q is not valid for type %q", key, target.Type)
		}
	}
	for _, pair := range [][2]string{{"token", "token_file"}, {"url", "url_file"}} {
		if !slices.Contains(allowed, pair[0]) {
			continue
		}
		hasValue, hasFile := keys.has("target", name, pair[0]), keys.has("target", name, pair[1])
		switch {
		case hasValue && hasFile:
			return fail(pair[1], "keys %q and %q are mutually exclusive", pair[0], pair[1])
		case !hasValue && !hasFile:
			return fail("", "one of keys %q and %q is required", pair[0], pair[1])
		}
	}
	for _, file := range []struct{ key, path string }{{"token_file", target.TokenFile}, {"url_file", target.URLFile}} {
		if keys.has("target", name, file.key) && !strings.HasPrefix(file.path, "/") {
			return fail(file.key, "key %q must be an absolute path", file.key)
		}
	}
	if keys.has("target", name, "preset") && !slices.Contains(presets, target.Preset) {
		return fail("preset", "unknown preset, want one of %s", strings.Join(presets, ", "))
	}
	if keys.has("target", name, "on_long") && !slices.Contains(onLongPolicies, target.OnLong) {
		return fail("on_long", "unknown policy, want one of %s", strings.Join(onLongPolicies, ", "))
	}
	return nil
}

func readSecretFiles(fsys fs.FS, cfg *Config, keys keyIndex) *Error {
	for _, name := range cfg.TargetNames() {
		target := cfg.Targets[name]
		for _, file := range []struct {
			key  string
			path string
			dst  *string
		}{{"token_file", target.TokenFile, &target.Token}, {"url_file", target.URLFile, &target.URL}} {
			if file.path == "" {
				continue
			}
			content, err := fs.ReadFile(fsys, strings.TrimPrefix(file.path, "/"))
			if err != nil {
				pos := keys.position("target", name, file.key)
				return &Error{Line: pos.Line, Column: pos.Column, Msg: fmt.Sprintf("target %q: %s: %v", name, file.key, err)}
			}
			*file.dst = strings.TrimSpace(string(content))
		}
		cfg.Targets[name] = target
	}
	return nil
}

// validateSecrets checks token and URL values after *_file references are
// resolved. A bad endpoint is a configuration error: at delivery time it
// would look like a network failure and be retried in vain.
func validateSecrets(cfg *Config, keys keyIndex) *Error {
	for _, name := range cfg.TargetNames() {
		target := cfg.Targets[name]
		allowed := allowedKeys[target.Type]
		fail := func(key, msg string) *Error {
			if !keys.has("target", name, key) {
				key += "_file"
			}
			pos := keys.position("target", name, key)
			return &Error{Line: pos.Line, Column: pos.Column, Msg: fmt.Sprintf("target %q: value of key %q %s", name, key, msg)}
		}
		if slices.Contains(allowed, "token") && strings.TrimSpace(target.Token) == "" {
			return fail("token", "is empty")
		}
		switch {
		case target.Type == TypeShoutrrr && !hasScheme(target.URL):
			return fail("url", "is not a shoutrrr service URL")
		case target.Type != TypeShoutrrr && slices.Contains(allowed, "url") && !isHTTPURL(target.URL):
			return fail("url", "is not an absolute http or https URL")
		}
	}
	return nil
}

func isHTTPURL(raw string) bool {
	parsed, err := url.Parse(raw)
	return err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != ""
}

// hasScheme accepts any URL with a scheme: shoutrrr selects the service by
// scheme (telegram://, gotify://, ...) and validates the rest itself.
func hasScheme(raw string) bool {
	parsed, err := url.Parse(raw)
	return err == nil && parsed.Scheme != ""
}

// TargetNames returns the target names in sorted order, so that callers do
// not depend on map iteration order.
func (c *Config) TargetNames() []string {
	names := make([]string, 0, len(c.Targets))
	for name := range c.Targets {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func sortedTypes() []string {
	types := make([]string, 0, len(allowedKeys))
	for t := range allowedKeys {
		types = append(types, t)
	}
	sort.Strings(types)
	return types
}
