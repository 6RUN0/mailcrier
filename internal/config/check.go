package config

import (
	"fmt"
	"io/fs"
	"net/url"
	"path"
	"slices"
	"sort"
	"strings"
	"syscall"

	"github.com/pelletier/go-toml/v2/unstable"
)

// SecretKeys are the target keys whose values may hold a secret: the
// caller hands them to its log redactor, and a file that writes one of
// them out is one to keep from other users.
var SecretKeys = []string{"token", "url", "headers", "query", "path"}

// Warning is a finding of Check about a configuration that loads but may
// not work as meant. Like Error it never quotes a configuration value.
type Warning struct {
	// Path is the absolute path of the configuration file.
	Path string
	// Line and Column locate the key the warning is about; 0 when it is
	// about the file as a whole.
	Line, Column int
	// Msg describes the problem.
	Msg string
}

// String returns "path:line:column: message", without the position when
// there is none, as Error.Error does.
func (w Warning) String() string {
	return (&Error{Path: w.Path, Line: w.Line, Column: w.Column, Msg: w.Msg}).Error()
}

// nativeTypes are the shoutrrr services that have a target type of their
// own, which knows the escaping, the length limits and the files of the
// service; the type has the name of the service.
var nativeTypes = []string{TypeTelegram, TypeSlack, TypeDiscord, TypeNtfy}

// discordHosts are the hosts of Discord webhooks.
var discordHosts = []string{"discord.com", "discordapp.com"}

// Check is Load followed by the checks of a configuration that loads but
// may not work as meant. group is the group of the setgid binary, through
// which every user other than root reads the files of the configuration,
// or -1 when the process did not get it: the checks of permissions then
// do not run. Permissions are checked by mode and owner group only,
// without access control lists. The warnings come sorted by position,
// those about a whole file last. Every returned error is *Error.
func Check(fsys fs.FS, path string, group int) (*Config, []Warning, error) {
	cfg, keys, err := loadWithKeys(fsys, path)
	if err != nil {
		return nil, nil, err
	}
	c := &checker{fsys: fsys, cfg: cfg, keys: keys, group: group, path: "/" + path}
	if group >= 0 {
		c.checkPermissions(path)
	}
	for _, name := range cfg.TargetNames() {
		c.checkTarget(name, cfg.Targets[name])
	}
	c.checkRoutes()
	sort.SliceStable(c.warnings, func(i, j int) bool {
		a, b := c.warnings[i], c.warnings[j]
		switch {
		case a.Line == 0 || b.Line == 0:
			return a.Line != 0 && b.Line == 0
		case a.Line != b.Line:
			return a.Line < b.Line
		default:
			return a.Column < b.Column
		}
	})
	return cfg, c.warnings, nil
}

// checker collects the warnings of one Check.
type checker struct {
	fsys     fs.FS
	cfg      *Config
	keys     keyIndex
	group    int
	path     string
	warnings []Warning
}

// add records a warning at pos, the zero position for the file as a
// whole.
func (c *checker) add(pos unstable.Position, format string, args ...any) {
	c.warnings = append(c.warnings, Warning{Path: c.path, Line: pos.Line, Column: pos.Column, Msg: fmt.Sprintf(format, args...)})
}

// unreadableByGroup ends the warnings about a file the group of the binary
// cannot read: every other user gets the configuration rejected.
const unreadableByGroup = "by the group of the binary, every call of another user exits 78"

// checkPermissions checks the configuration file at name and the files it
// names against the group of the binary.
func (c *checker) checkPermissions(name string) {
	if info, err := fs.Stat(c.fsys, name); err == nil && info.Mode().Perm()&0o004 != 0 && c.hasSecretKey() {
		c.add(unstable.Position{}, "file is readable by all users and holds secrets")
	}
	switch c.blockedPart(name) {
	case "file":
		c.add(unstable.Position{}, "file is not readable "+unreadableByGroup)
	case "directory":
		c.add(unstable.Position{}, "directory of the file is not searchable "+unreadableByGroup)
	}
	for _, target := range c.cfg.TargetNames() {
		settings := c.cfg.Targets[target]
		for _, file := range []struct {
			key, path    string
			isSecretFile bool
		}{{"token_file", settings.TokenFile, true}, {"url_file", settings.URLFile, true}, {"template_file", settings.TemplateFile, false}} {
			if file.path == "" {
				continue
			}
			pos := c.keys.position("target", target, file.key)
			name := strings.TrimPrefix(file.path, "/")
			if info, err := fs.Stat(c.fsys, name); err == nil && file.isSecretFile && info.Mode().Perm()&0o004 != 0 {
				c.add(pos, "target %q: file of key %q is readable by all users", target, file.key)
			}
			switch c.blockedPart(name) {
			case "file":
				c.add(pos, "target %q: file of key %q is not readable "+unreadableByGroup, target, file.key)
			case "directory":
				c.add(pos, "target %q: directory of key %q is not searchable "+unreadableByGroup, target, file.key)
			}
		}
	}
}

// hasSecretKey reports whether a target writes out a key of SecretKeys.
func (c *checker) hasSecretKey() bool {
	for _, target := range c.cfg.TargetNames() {
		for _, key := range SecretKeys {
			if c.keys.has("target", target, key) {
				return true
			}
		}
	}
	return false
}

// blockedPart returns what keeps the group of the binary from the file at
// name: "file" when it cannot read the file, "directory" when it cannot
// search a directory on the way to it, the root of fsys included, and ""
// otherwise. A file or directory that cannot be stated is not reported.
func (c *checker) blockedPart(name string) string {
	info, err := fs.Stat(c.fsys, name)
	if err != nil {
		return ""
	}
	if !c.isGranted(info, 0o004, 0o040) {
		return "file"
	}
	for dir := path.Dir(name); ; dir = path.Dir(dir) {
		if info, err := fs.Stat(c.fsys, dir); err == nil && !c.isGranted(info, 0o001, 0o010) {
			return "directory"
		}
		if dir == "." {
			return ""
		}
	}
}

// isGranted reports whether the group of the binary has a permission on
// info. The kernel applies one class of bits: those of the group when the
// file belongs to that group, those for all users otherwise, so 0604 of
// the group denies a read that 0604 of another group allows. The owner
// class is left out, as the caller is another user than the owner. Without
// the owner group, which only a stat of the system gives, it reports
// true.
func (c *checker) isGranted(info fs.FileInfo, forAll, forGroup fs.FileMode) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return true
	}
	perm := info.Mode().Perm()
	if int(stat.Gid) == c.group {
		return perm&forGroup != 0
	}
	return perm&forAll != 0
}

// checkTarget checks one target for settings that load but defeat their
// purpose.
func (c *checker) checkTarget(name string, target Target) {
	switch target.Type {
	case TypeShoutrrr:
		parsed, err := url.Parse(target.URL)
		if err != nil {
			return
		}
		service, _, _ := strings.Cut(strings.ToLower(parsed.Scheme), "+")
		if slices.Contains(nativeTypes, service) {
			c.add(c.secretPosition(name, "url"), "target %q: shoutrrr service %q has a native target type %q: escaping, length limits and files", name, service, service)
		}
	case TypeHTTP:
		parsed, err := url.Parse(target.URL)
		if err != nil || target.Preset != PresetSlackWebhook {
			return
		}
		host := strings.ToLower(parsed.Hostname())
		if slices.ContainsFunc(discordHosts, func(discord string) bool { return host == discord || strings.HasSuffix(host, "."+discord) }) {
			c.add(c.keys.position("target", name, "preset"), "target %q: preset %q on a Discord host does not disable mentions, use type %q", name, PresetSlackWebhook, TypeDiscord)
		}
	case TypeExec:
		info, err := fs.Stat(c.fsys, strings.TrimPrefix(target.Argv[0], "/"))
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			c.add(c.keys.position("target", name, "argv"), "target %q: first element of key %q is not an executable file", name, "argv")
		}
	}
}

// secretPosition returns the position of key, or of its _file variant
// when the file uses that one.
func (c *checker) secretPosition(target, key string) unstable.Position {
	if !c.keys.has("target", target, key) {
		key += "_file"
	}
	return c.keys.position("target", target, key)
}

// checkRoutes checks that the rules leave no message and no target
// without a way: a message that no rule matches is held, and a target
// that no rule names gets nothing.
func (c *checker) checkRoutes() {
	if len(c.cfg.Routes) == 0 {
		return
	}
	if !slices.ContainsFunc(c.cfg.Routes, hasNoConditions) {
		c.add(c.keys.position(elementPath("route", 0)...), "routes have no rule without conditions: a message no rule matches is held and the call exits 64, or the message is lost with the spool off")
	}
	for _, name := range c.cfg.TargetNames() {
		if name == c.cfg.General.TelegramDirect || slices.ContainsFunc(c.cfg.Routes, func(r Route) bool { return slices.Contains(r.Targets, name) }) {
			continue
		}
		c.add(c.keys.position("target", name), "target %q: no route names this target", name)
	}
}

// hasNoConditions reports a rule that matches every message. Match,
// filled by compileRules, holds every condition a rule can have.
func hasNoConditions(r Route) bool {
	return r.Match == (Match{})
}
