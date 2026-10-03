package config

import (
	"errors"
	"io/fs"
	"slices"
	"strings"
	"syscall"
	"testing"
	"testing/fstest"
)

// binaryGroup is the group of the setgid binary in the cases below.
const binaryGroup = 990

// owned returns a file of mode owned by the group gid.
func owned(mode fs.FileMode, gid uint32, data string) *fstest.MapFile {
	return &fstest.MapFile{Data: []byte(data), Mode: mode, Sys: &syscall.Stat_t{Gid: gid}}
}

// tgTokenFile is a telegram target whose token is in a file.
const tgTokenFile = "[target.tg]\ntype = \"telegram\"\ntoken_file = \"/etc/mailcrier.d/tg.token\"\nchat_id = 1\n"

// checkCase runs Check on doc, held in a file of configMode owned by the
// binary group, with files beside it.
type checkCase struct {
	doc        string
	configMode fs.FileMode
	files      fstest.MapFS
	group      int
	want       []string
}

func (tc checkCase) run(t *testing.T) []Warning {
	t.Helper()
	mode := tc.configMode
	if mode == 0 {
		mode = 0o640
	}
	fsys := fstest.MapFS{configPath: owned(mode, binaryGroup, tc.doc)}
	for name, file := range tc.files {
		fsys[name] = file
	}
	_, warnings, err := Check(fsys, configPath, tc.group)
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	return warnings
}

func (tc checkCase) check(t *testing.T) {
	t.Helper()
	var got []string
	for _, w := range tc.run(t) {
		got = append(got, strings.TrimPrefix(w.String(), "/"+configPath))
	}
	if !slices.Equal(got, tc.want) {
		t.Errorf("warnings\n got  %q\n want %q", got, tc.want)
	}
}

func TestCheckWarnings(t *testing.T) {
	token := func(mode fs.FileMode, gid uint32) fstest.MapFS {
		return fstest.MapFS{"etc/mailcrier.d/tg.token": owned(mode, gid, "123:abc\n")}
	}
	const (
		tokenWorldReadable = `:3:1: target "tg": file of key "token_file" is readable by all users`
		tokenUnreadable    = `:3:1: target "tg": file of key "token_file" is not readable by the group of the binary, every call of another user exits 78`
		tokenDirBlocked    = `:3:1: target "tg": directory of key "token_file" is not searchable by the group of the binary, every call of another user exits 78`
		configWithSecrets  = `: file is readable by all users and holds secrets`
	)
	cases := []struct {
		name string
		checkCase
	}{
		{"clean", checkCase{doc: tgTokenFile, files: token(0o640, binaryGroup), group: binaryGroup}},
		{"secret-file-readable-by-all", checkCase{doc: tgTokenFile, files: token(0o644, binaryGroup), group: binaryGroup, want: []string{tokenWorldReadable}}},
		{"secret-file-readable-by-all-unelevated", checkCase{doc: tgTokenFile, files: token(0o644, binaryGroup), group: -1}},
		{"secret-file-of-other-group-readable-by-all", checkCase{doc: tgTokenFile, files: token(0o644, 0), group: binaryGroup, want: []string{tokenWorldReadable}}},
		{"secret-file-without-group-read", checkCase{doc: tgTokenFile, files: token(0o600, binaryGroup), group: binaryGroup, want: []string{tokenUnreadable}}},
		{"secret-file-of-other-group", checkCase{doc: tgTokenFile, files: token(0o640, 0), group: binaryGroup, want: []string{tokenUnreadable}}},
		{"secret-file-of-group-readable-by-others-only", checkCase{doc: tgTokenFile, files: token(0o604, binaryGroup), group: binaryGroup, want: []string{tokenWorldReadable, tokenUnreadable}}},
		{"directory-searchable-by-others-only", checkCase{doc: tgTokenFile, files: fstest.MapFS{
			"etc/mailcrier.d":          owned(fs.ModeDir|0o701, binaryGroup, ""),
			"etc/mailcrier.d/tg.token": owned(0o640, binaryGroup, "123:abc\n"),
		}, group: binaryGroup, want: []string{tokenDirBlocked}}},
		{"secret-file-of-other-group-unelevated", checkCase{doc: tgTokenFile, files: token(0o640, 0), group: -1}},
		{"directory-closed-to-group", checkCase{doc: tgTokenFile, files: fstest.MapFS{
			"etc/mailcrier.d":          owned(fs.ModeDir|0o700, 0, ""),
			"etc/mailcrier.d/tg.token": owned(0o640, binaryGroup, "123:abc\n"),
		}, group: binaryGroup, want: []string{tokenDirBlocked}}},
		{"directory-open-to-group", checkCase{doc: tgTokenFile, files: fstest.MapFS{
			"etc/mailcrier.d":          owned(fs.ModeDir|0o750, binaryGroup, ""),
			"etc/mailcrier.d/tg.token": owned(0o640, binaryGroup, "123:abc\n"),
		}, group: binaryGroup}},
		{"root-closed-to-group", checkCase{doc: tgTokenFile, files: fstest.MapFS{
			".":                        owned(fs.ModeDir|0o700, 0, ""),
			"etc/mailcrier.d/tg.token": owned(0o640, binaryGroup, "123:abc\n"),
		}, group: binaryGroup, want: []string{tokenDirBlocked, `: directory of the file is not searchable by the group of the binary, every call of another user exits 78`}}},
		{"template-file-of-other-group", checkCase{
			doc:   "[target.dc]\ntype = \"discord\"\nurl = \"https://example.org/x\"\ntemplate_file = \"/etc/mailcrier.d/dc.tmpl\"\n",
			files: fstest.MapFS{"etc/mailcrier.d/dc.tmpl": owned(0o640, 0, "{{ .Subject }}\n")}, group: binaryGroup,
			want: []string{`:4:1: target "dc": file of key "template_file" is not readable by the group of the binary, every call of another user exits 78`},
		}},
		{"template-file-readable-by-all", checkCase{
			doc:   "[target.dc]\ntype = \"discord\"\nurl_file = \"/etc/mailcrier.d/dc.url\"\ntemplate_file = \"/etc/mailcrier.d/dc.tmpl\"\n",
			files: fstest.MapFS{"etc/mailcrier.d/dc.tmpl": owned(0o644, 0, "{{ .Subject }}\n"), "etc/mailcrier.d/dc.url": owned(0o640, binaryGroup, "https://example.org/x\n")}, group: binaryGroup,
		}},
		{"config-of-other-group", checkCase{doc: tgTokenFile, configMode: 0o600, files: token(0o640, binaryGroup), group: binaryGroup,
			want: []string{`: file is not readable by the group of the binary, every call of another user exits 78`}}},
		{"config-with-token-readable-by-all", checkCase{doc: "[target.tg]\ntype = \"telegram\"\ntoken = \"1:a\"\nchat_id = 1\n", configMode: 0o644, group: binaryGroup, want: []string{configWithSecrets}}},
		{"config-with-url-readable-by-all", checkCase{doc: "[target.api]\ntype = \"http\"\npreset = \"generic-json\"\nurl = \"https://example.org/x\"\n", configMode: 0o644, group: binaryGroup, want: []string{configWithSecrets}}},
		{"config-with-headers-readable-by-all", checkCase{doc: "[target.api]\ntype = \"http\"\npreset = \"generic-json\"\nurl_file = \"/etc/mailcrier.d/api.url\"\nheaders = { Authorization = \"Bearer x\" }\n",
			configMode: 0o644, files: fstest.MapFS{"etc/mailcrier.d/api.url": owned(0o640, binaryGroup, "https://example.org/x\n")}, group: binaryGroup, want: []string{configWithSecrets}}},
		{"config-with-query-readable-by-all", checkCase{doc: "[target.api]\ntype = \"http\"\npreset = \"generic-json\"\nurl_file = \"/etc/mailcrier.d/api.url\"\nquery = { key = \"x\" }\n",
			configMode: 0o644, files: fstest.MapFS{"etc/mailcrier.d/api.url": owned(0o640, binaryGroup, "https://example.org/x\n")}, group: binaryGroup, want: []string{configWithSecrets}}},
		{"config-with-path-readable-by-all", checkCase{doc: "[target.api]\ntype = \"http\"\npreset = \"generic-json\"\nurl_file = \"/etc/mailcrier.d/api.url\"\npath = \"/x\"\n",
			configMode: 0o644, files: fstest.MapFS{"etc/mailcrier.d/api.url": owned(0o640, binaryGroup, "https://example.org/x\n")}, group: binaryGroup, want: []string{configWithSecrets}}},
		{"config-without-secrets-readable-by-all", checkCase{doc: tgTokenFile, configMode: 0o644, files: token(0o640, binaryGroup), group: binaryGroup}},
		{"config-with-token-readable-by-all-unelevated", checkCase{doc: "[target.tg]\ntype = \"telegram\"\ntoken = \"1:a\"\nchat_id = 1\n", configMode: 0o644, group: -1}},
		{"shoutrrr-telegram", checkCase{doc: "[target.x]\ntype = \"shoutrrr\"\nurl = \"telegram://1:a@telegram?chats=1\"\n", group: -1,
			want: []string{`:3:1: target "x": shoutrrr service "telegram" has a native target type "telegram": escaping, length limits and files`}}},
		{"shoutrrr-telegram-variant", checkCase{doc: "[target.x]\ntype = \"shoutrrr\"\n\nurl_file = \"/etc/mailcrier.d/x.url\"\n",
			files: fstest.MapFS{"etc/mailcrier.d/x.url": owned(0o640, binaryGroup, "Telegram+https://1:a@telegram?chats=1\n")}, group: -1,
			want: []string{`:4:1: target "x": shoutrrr service "telegram" has a native target type "telegram": escaping, length limits and files`}}},
		{"shoutrrr-gotify", checkCase{doc: "[target.x]\ntype = \"shoutrrr\"\nurl = \"gotify://example.org/token\"\n", group: -1}},
		{"slack-webhook-on-discord", checkCase{doc: "[target.x]\ntype = \"http\"\nurl = \"https://discord.com/api/webhooks/1/a/slack\"\npreset = \"slack-webhook\"\n", group: -1,
			want: []string{`:4:1: target "x": preset "slack-webhook" on a Discord host does not disable mentions, use type "discord"`}}},
		{"slack-webhook-on-discord-subdomain", checkCase{doc: "[target.x]\ntype = \"http\"\nurl = \"https://PTB.discord.com/api/webhooks/1/a/slack\"\npreset = \"slack-webhook\"\n", group: -1,
			want: []string{`:4:1: target "x": preset "slack-webhook" on a Discord host does not disable mentions, use type "discord"`}}},
		{"slack-webhook-elsewhere", checkCase{doc: "[target.x]\ntype = \"http\"\nurl = \"https://notdiscord.com/hook\"\npreset = \"slack-webhook\"\n", group: -1}},
		{"route-without-conditions", checkCase{doc: tgTokenFile + "[[route]]\nsubject = \"x\"\ntargets = [\"tg\"]\n[[route]]\ntargets = [\"tg\"]\n", files: token(0o640, binaryGroup), group: -1}},
		{"routes-all-with-conditions", checkCase{doc: tgTokenFile + "\n[[route]]\nsubject = \"*\"\ntargets = [\"tg\"]\n", files: token(0o640, binaryGroup), group: -1,
			want: []string{`:6:3: routes have no rule without conditions: a message no rule matches is held and the call exits 64, or the message is lost with the spool off`}}},
		{"routes-with-recipient-regex-only", checkCase{doc: tgTokenFile + "\n[[route]]\nrecipient_regex = \".\"\ntargets = [\"tg\"]\n", files: token(0o640, binaryGroup), group: -1,
			want: []string{`:6:3: routes have no rule without conditions: a message no rule matches is held and the call exits 64, or the message is lost with the spool off`}}},
		{"target-without-route", checkCase{doc: tgTokenFile + "[target.dc]\ntype = \"discord\"\nurl = \"https://example.org/x\"\n[[route]]\ntargets = [\"tg\"]\n", files: token(0o640, binaryGroup), group: -1,
			want: []string{`:5:9: target "dc": no route names this target`}}},
		{"direct-target-without-route", checkCase{doc: "[general]\ntelegram_direct = \"tg\"\ntelegram_direct_chats = [1]\n" + tgTokenFile + "[target.dc]\ntype = \"discord\"\nurl = \"https://example.org/x\"\n[[route]]\ntargets = [\"dc\"]\n",
			files: token(0o640, binaryGroup), group: -1}},
		{"hook-missing", checkCase{doc: "[target.run]\ntype = \"exec\"\nargv = [\"/usr/local/bin/notify\"]\n", group: -1,
			want: []string{`:3:1: target "run": first element of key "argv" is not an executable file`}}},
		{"hook-not-executable", checkCase{doc: "[target.run]\ntype = \"exec\"\nargv = [\"/usr/local/bin/notify\"]\n", files: fstest.MapFS{"usr/local/bin/notify": owned(0o644, 0, "#!/bin/sh\n")}, group: -1,
			want: []string{`:3:1: target "run": first element of key "argv" is not an executable file`}}},
		{"hook-is-directory", checkCase{doc: "[target.run]\ntype = \"exec\"\nargv = [\"/usr/local/bin/notify\"]\n", files: fstest.MapFS{"usr/local/bin/notify": owned(fs.ModeDir|0o755, 0, "")}, group: -1,
			want: []string{`:3:1: target "run": first element of key "argv" is not an executable file`}}},
		{"hook-executable", checkCase{doc: "[target.run]\ntype = \"exec\"\nargv = [\"/usr/local/bin/notify\"]\n", files: fstest.MapFS{"usr/local/bin/notify": owned(0o755, 0, "#!/bin/sh\n")}, group: -1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, tc.check)
	}
}

// TestCheckWarningsOmitValues pins that no warning quotes a value of the
// file, as no error does: the warnings of a file full of them name keys
// only.
func TestCheckWarningsOmitValues(t *testing.T) {
	const marker = "SECRET"
	doc := `[target.tg]
type = "telegram"
token_file = "/etc/mailcrier.d/SECRET.token"
chat_id = 1

[target.x]
type = "shoutrrr"
url = "telegram://1:SECRET@telegram?chats=1"

[target.api]
type = "http"
url = "https://SECRET.discord.com/api/webhooks/1/SECRET/slack"
preset = "slack-webhook"
headers = { Authorization = "Bearer SECRET-SECRET" }
query = { key = "SECRET-SECRET" }
path = "/SECRET"

[target.run]
type = "exec"
argv = ["/usr/local/bin/SECRET"]

[[route]]
subject = "SECRET"
targets = ["tg"]
`
	warnings := checkCase{doc: doc, configMode: 0o644, files: fstest.MapFS{"etc/mailcrier.d/SECRET.token": owned(0o604, 0, "123:abc\n")}, group: binaryGroup}.run(t)
	if len(warnings) < 7 {
		t.Fatalf("Check() gave %d warnings, want every kind: %q", len(warnings), warnings)
	}
	for _, w := range warnings {
		if strings.Contains(w.Msg, marker) {
			t.Errorf("warning quotes a value: %s", w)
		}
	}
}

// TestCheckPathIsAbsolute pins that errors and warnings name the file by
// its absolute path, whatever fsys it was read from.
func TestCheckPathIsAbsolute(t *testing.T) {
	_, _, err := Check(fstest.MapFS{configPath: {Data: []byte("bogus = 1\n")}}, configPath, -1)
	var cfgErr *Error
	if !errors.As(err, &cfgErr) || cfgErr.Path != "/"+configPath {
		t.Errorf("Check() error = %v, want *Error with Path /%s", err, configPath)
	}
	_, warnings, err := Check(fstest.MapFS{configPath: {Data: []byte("[target.run]\ntype = \"exec\"\nargv = [\"/x\"]\n")}}, configPath, -1)
	if err != nil || len(warnings) != 1 || warnings[0].Path != "/"+configPath {
		t.Errorf("Check() = %v, %v, want one warning with Path /%s", warnings, err, configPath)
	}
}
