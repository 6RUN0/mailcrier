package config

import (
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

const configPath = "etc/mailcrier.conf"

func load(t *testing.T, doc string, extra fstest.MapFS) (*Config, error) {
	t.Helper()
	fsys := fstest.MapFS{configPath: {Data: []byte(doc)}}
	for name, file := range extra {
		fsys[name] = file
	}
	return Load(fsys, configPath)
}

func TestLoadValid(t *testing.T) {
	doc := `
[general]
syslog_tag = "mail-notify"

[target.hook]
type = "http"
url_file = "/etc/mailcrier.d/hook.url"
preset = "generic-json"
method = "PUT"

[target.ops-telegram]
type = "telegram"
token = "123:abc"
chat_id = "-100123"
on_long = "blockquote"
long_file = "eml"
max_text = 1000
max_lines = 40
max_file_size = 2000000

[target.dc]
type = "discord"
url = "https://example.org/x"
max_file_size = 3000000

[target.sl]
type = "slack"
token = "xoxb-1"
channel = "#ops"
max_file_size = 4000000

[target.run]
type = "exec"
argv = ["/usr/local/bin/notify", "--to", "ops"]
timeout = "10s"
`
	secret := fstest.MapFS{"etc/mailcrier.d/hook.url": {Data: []byte("https://example.org/hooks/secret\n")}}
	cfg, err := load(t, doc, secret)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.General.SyslogTag != "mail-notify" {
		t.Errorf("SyslogTag = %q", cfg.General.SyslogTag)
	}
	hook := cfg.Targets["hook"]
	if hook.Type != TypeHTTP || hook.Preset != PresetGenericJSON || hook.Method != "PUT" {
		t.Errorf("hook = %+v", hook)
	}
	if hook.URL != "https://example.org/hooks/secret" {
		t.Errorf("hook.URL = %q, want the trimmed content of url_file", hook.URL)
	}
	if tg := cfg.Targets["ops-telegram"]; tg.Token != "123:abc" || tg.ChatID != "-100123" || tg.OnLong != OnLongBlockquote || tg.LongFile != LongFileMessage || tg.MaxText != 1000 || tg.MaxLines != 40 || tg.MaxFileSize != 2000000 {
		t.Errorf("ops-telegram = %+v", tg)
	}
	if run := cfg.Targets["run"]; strings.Join(run.Argv, " ") != "/usr/local/bin/notify --to ops" || run.Timeout.Duration != 10*time.Second {
		t.Errorf("run = %+v", run)
	}
	if dc, sl := cfg.Targets["dc"], cfg.Targets["sl"]; dc.MaxFileSize != 3000000 || sl.MaxFileSize != 4000000 {
		t.Errorf("max_file_size of discord %d, of slack %d", dc.MaxFileSize, sl.MaxFileSize)
	}
}

// TestLoadAcceptsServiceURLs pins that the URL rule depends on the target
// type: http-based targets need http or https, shoutrrr takes its own
// service schemes.
func TestLoadAcceptsServiceURLs(t *testing.T) {
	docs := map[string]string{
		"shoutrrr": "[target.bus]\ntype = \"shoutrrr\"\nurl = \"telegram://123:abc@telegram?chats=@ops\"\n",
		"http":     "[target.hook]\ntype = \"http\"\npreset = \"generic-json\"\nurl = \"https://hooks.example.org/in\"\n",
		"ntfy":     "[target.push]\ntype = \"ntfy\"\nurl = \"https://ntfy.sh/topic\"\n",
	}
	for name, doc := range docs {
		if _, err := load(t, doc, nil); err != nil {
			t.Errorf("%s: Load() error = %v", name, err)
		}
	}
}

func TestLoadDefaultsSyslogTag(t *testing.T) {
	cfg, err := load(t, "[target.hook]\ntype = \"http\"\npreset = \"generic-json\"\nurl = \"https://example.org\"\n", nil)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.General.SyslogTag != DefaultSyslogTag {
		t.Errorf("SyslogTag = %q, want %q", cfg.General.SyslogTag, DefaultSyslogTag)
	}
}

func TestLoadRejects(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		// want is the expected error text after the file path.
		want string
	}{
		{
			name: "unknown-key",
			doc:  "[target.hook]\ntype = \"http\"\npreset = \"generic-json\"\nurl = \"https://example.org\"\ncolour = \"red\"\n",
			want: `:5:1: unknown key "target.hook.colour"`,
		},
		{
			name: "unknown-top-level-table",
			doc:  "[target.hook]\ntype = \"http\"\npreset = \"generic-json\"\nurl = \"https://example.org\"\n\n[daemon]\nlisten = \"127.0.0.1:25\"\n",
			want: `:6:2: unknown key "daemon"`,
		},
		{
			name: "key-of-other-target-type",
			doc:  "[target.hook]\ntype = \"http\"\npreset = \"generic-json\"\nurl = \"https://example.org\"\nchat_id = \"-100123\"\n",
			want: `:5:1: target "hook": key "chat_id" is not valid for type "http"`,
		},
		{
			name: "http-channel-without-mattermost",
			doc:  "[target.hook]\ntype = \"http\"\npreset = \"generic-json\"\nurl = \"https://example.org\"\nchannel = \"alerts\"\n",
			want: `:5:1: target "hook": key "channel" needs preset "mattermost"`,
		},
		{
			name: "http-header-name-invalid",
			doc:  "[target.hook]\ntype = \"http\"\npreset = \"generic-json\"\nurl = \"https://example.org\"\n\n[target.hook.headers]\n\"X Token\" = \"SECRET\"\n",
			want: `:7:1: target "hook": header name "X Token" is invalid`,
		},
		{
			name: "http-header-value-line-break",
			doc:  "[target.hook]\ntype = \"http\"\npreset = \"generic-json\"\nurl = \"https://example.org\"\n\n[target.hook.headers]\nAuthorization = \"Bearer SECRET\\r\\nX-Evil: 1\"\n",
			want: `:7:1: target "hook": value of header "Authorization" is invalid`,
		},
		{
			name: "http-header-value-nul",
			doc:  "[target.hook]\ntype = \"http\"\npreset = \"generic-json\"\nurl = \"https://example.org\"\n\n[target.hook.headers]\nX-Token = \"SECRET\\u0000\"\n",
			want: `:7:1: target "hook": value of header "X-Token" is invalid`,
		},
		{
			name: "get-with-preset",
			doc:  "[target.hook]\ntype = \"http\"\npreset = \"generic-json\"\nurl = \"https://example.org\"\nmethod = \"GET\"\n",
			want: `:3:1: target "hook": key "preset" is not valid with method "GET"`,
		},
		{
			name: "get-with-template",
			doc:  "[target.hook]\ntype = \"http\"\nurl = \"https://example.org\"\nmethod = \"GET\"\ntemplate = \"{}\"\n",
			want: `:5:1: target "hook": key "template" is not valid with method "GET"`,
		},
		{
			name: "get-with-max-text",
			doc:  "[target.hook]\ntype = \"http\"\nurl = \"https://example.org\"\nmethod = \"GET\"\nmax_text = 10\n",
			want: `:5:1: target "hook": key "max_text" is not valid with method "GET"`,
		},
		{
			name: "path-blank",
			doc:  "[target.hook]\ntype = \"http\"\nurl = \"https://example.org\"\nmethod = \"GET\"\npath = \" \"\n",
			want: `:5:1: target "hook": value of key "path" must not be blank`,
		},
		{
			name: "query-name-empty",
			doc:  "[target.hook]\ntype = \"http\"\nurl = \"https://example.org\"\nmethod = \"GET\"\n[target.hook.query]\n\"\" = \"x\"\n",
			want: `:6:1: target "hook": query name must not be empty`,
		},
		{
			name: "get-with-max-lines",
			doc:  "[target.hook]\ntype = \"http\"\nurl = \"https://example.org\"\nmethod = \"GET\"\nmax_lines = 10\n",
			want: `:5:1: target "hook": key "max_lines" is not valid with method "GET"`,
		},
		{
			name: "path-without-slash",
			doc:  "[target.hook]\ntype = \"http\"\nurl = \"https://example.org\"\nmethod = \"GET\"\npath = \"hooks/{{ .Hostname }}\"\n",
			want: `:5:1: target "hook": value of key "path" must start with a single /`,
		},
		{
			name: "path-other-host",
			doc:  "[target.hook]\ntype = \"http\"\nurl = \"https://example.org\"\nmethod = \"GET\"\npath = \"//evil.example/{{ .Hostname }}\"\n",
			want: `:5:1: target "hook": value of key "path" must start with a single /`,
		},
		{
			name: "path-dot-dot",
			doc:  "[target.hook]\ntype = \"http\"\nurl = \"https://example.org\"\nmethod = \"GET\"\npath = \"/a/..;x/{{ .Hostname }}\"\n",
			want: `:5:1: target "hook": value of key "path" must not hold a . or .. segment`,
		},
		{
			name: "path-double-escaped-dot-dot",
			doc:  "[target.hook]\ntype = \"http\"\nurl = \"https://example.org\"\nmethod = \"GET\"\npath = \"/a/%252e%252e/b\"\n",
			want: `:5:1: target "hook": value of key "path" must not hold a . or .. segment`,
		},
		{
			name: "path-double-escaped-dot-dot-bad-parameter",
			doc:  "[target.hook]\ntype = \"http\"\nurl = \"https://example.org\"\nmethod = \"GET\"\npath = \"/x/%252e%252e;%25zz/y\"\n",
			want: `:5:1: target "hook": value of key "path" must not hold a . or .. segment`,
		},
		{
			name: "path-bad-escape",
			doc:  "[target.hook]\ntype = \"http\"\nurl = \"https://example.org\"\nmethod = \"GET\"\npath = \"/a%zz/{{ .Hostname }}\"\n",
			want: `:5:1: target "hook": value of key "path" must not hold an invalid escape`,
		},
		{
			name: "path-control-character",
			doc:  "[target.hook]\ntype = \"http\"\nurl = \"https://example.org\"\nmethod = \"GET\"\npath = \"/a\\u0001/{{ .Hostname }}\"\n",
			want: `:5:1: target "hook": value of key "path" must not hold a control character`,
		},
		{
			name: "path-escaped-control-character",
			doc:  "[target.hook]\ntype = \"http\"\nurl = \"https://example.org\"\nmethod = \"GET\"\npath = \"/a%0A/{{ .Hostname }}\"\n",
			want: `:5:1: target "hook": value of key "path" must not hold a control character or a backslash, escaped or not`,
		},
		{
			name: "path-query",
			doc:  "[target.hook]\ntype = \"http\"\nurl = \"https://example.org\"\nmethod = \"GET\"\npath = \"/a?b={{ .Hostname }}\"\n",
			want: `:5:1: target "hook": value of key "path" must not hold ?, # or a backslash`,
		},
		{
			name: "path-of-other-type",
			doc:  "[target.dc]\ntype = \"discord\"\nurl = \"https://example.org\"\npath = \"/x\"\n",
			want: `:4:1: target "dc": key "path" is not valid for type "discord"`,
		},
		{
			name: "http-method-lowercase",
			doc:  "[target.hook]\ntype = \"http\"\npreset = \"generic-json\"\nurl = \"https://example.org\"\nmethod = \"put\"\n",
			want: `:5:1: target "hook": unknown method, want one of GET, POST, PUT, PATCH`,
		},
		{
			name: "exec-argv-missing",
			doc:  "[target.run]\ntype = \"exec\"\ntimeout = \"5s\"\n",
			want: `:1:9: target "run": key "argv" is required`,
		},
		{
			name: "exec-argv-empty",
			doc:  "[target.run]\ntype = \"exec\"\nargv = []\n",
			want: `:3:1: target "run": value of key "argv" is empty`,
		},
		{
			name: "exec-argv-relative",
			doc:  "[target.run]\ntype = \"exec\"\nargv = [\"notify.sh\", \"SECRET\"]\n",
			want: `:3:1: target "run": first element of key "argv" must be an absolute path`,
		},
		{
			name: "exec-argv-nul",
			doc:  "[target.run]\ntype = \"exec\"\nargv = [\"/usr/local/bin/notify\", \"SECRET\\u0000\"]\n",
			want: `:3:1: target "run": value of key "argv" holds a NUL character`,
		},
		{
			name: "exec-timeout-zero",
			doc:  "[target.run]\ntype = \"exec\"\nargv = [\"/usr/local/bin/notify\"]\ntimeout = \"0s\"\n",
			want: `:4:1: target "run": value of key "timeout" must be positive`,
		},
		{
			name: "exec-url",
			doc:  "[target.run]\ntype = \"exec\"\nargv = [\"/usr/local/bin/notify\"]\nurl = \"https://example.org\"\n",
			want: `:4:1: target "run": key "url" is not valid for type "exec"`,
		},
		{
			name: "http-preset-missing",
			doc:  "[target.hook]\ntype = \"http\"\nurl = \"https://example.org\"\n",
			want: `:1:9: target "hook": one of keys "preset", "template" and "template_file" is required`,
		},
		{
			name: "http-preset-and-template",
			doc:  "[target.hook]\ntype = \"http\"\nurl = \"https://example.org\"\ntemplate = \"{}\"\npreset = \"generic-json\"\n",
			want: `:5:1: target "hook": key "preset" and a template are mutually exclusive`,
		},
		{
			name: "template-and-template-file",
			doc:  "[target.tg]\ntype = \"telegram\"\ntoken = \"1:a\"\nchat_id = 1\ntemplate = \"x\"\ntemplate_file = \"/etc/t.tmpl\"\n",
			want: `:6:1: target "tg": keys "template" and "template_file" are mutually exclusive`,
		},
		{
			name: "template-blank",
			doc:  "[target.tg]\ntype = \"telegram\"\ntoken = \"1:a\"\nchat_id = 1\ntemplate = \" \\n\"\n",
			want: `:5:1: target "tg": value of key "template" must not be blank`,
		},
		{
			name: "template-file-relative",
			doc:  "[target.dc]\ntype = \"discord\"\nurl = \"https://example.org\"\ntemplate_file = \"t.tmpl\"\n",
			want: `:4:1: target "dc": key "template_file" must be an absolute path`,
		},
		{
			name: "template-file-missing",
			doc:  "[target.dc]\ntype = \"discord\"\nurl = \"https://example.org\"\ntemplate_file = \"/etc/t.tmpl\"\n",
			want: `:4:1: target "dc": template_file: /etc/t.tmpl: file does not exist`,
		},
		{
			name: "exec-template",
			doc:  "[target.run]\ntype = \"exec\"\nargv = [\"/usr/local/bin/notify\"]\ntemplate = \"x\"\n",
			want: `:4:1: target "run": key "template" is not valid for type "exec"`,
		},
		{
			name: "invalid-name",
			doc:  "[target.Ops_TG]\ntype = \"http\"\npreset = \"generic-json\"\nurl = \"https://example.org\"\n",
			want: `:1:9: target "Ops_TG": name must match ^[a-z0-9-]+$`,
		},
		{
			name: "token-and-token-file",
			doc:  "[target.tg]\ntype = \"telegram\"\ntoken = \"123:abc\"\ntoken_file = \"/etc/mailcrier.d/tg.token\"\nchat_id = \"1\"\n",
			want: `:4:1: target "tg": keys "token" and "token_file" are mutually exclusive`,
		},
		{
			name: "neither-url-nor-url-file",
			doc:  "\n[target.hook]\ntype = \"http\"\npreset = \"generic-json\"\n",
			want: `:2:9: target "hook": one of keys "url" and "url_file" is required`,
		},
		{
			name: "missing-type",
			doc:  "[target.hook]\nurl = \"https://example.org\"\n",
			want: `:1:9: target "hook": key "type" is required`,
		},
		{
			name: "unknown-type",
			doc:  "[target.hook]\ntype = \"pigeon\"\n",
			want: `:2:1: target "hook": unknown type, want one of discord, exec, http, ntfy, shoutrrr, slack, telegram`,
		},
		{
			name: "unknown-preset",
			doc:  "[target.hook]\ntype = \"http\"\nurl = \"https://example.org\"\npreset = \"xml\"\n",
			want: `:4:1: target "hook": unknown preset, want one of mattermost, slack-webhook, generic-json`,
		},
		{
			name: "relative-secret-file",
			doc:  "[target.hook]\ntype = \"http\"\npreset = \"generic-json\"\nurl_file = \"hook.url\"\n",
			want: `:4:1: target "hook": key "url_file" must be an absolute path`,
		},
		{
			name: "missing-secret-file",
			doc:  "[target.hook]\ntype = \"http\"\npreset = \"generic-json\"\nurl_file = \"/etc/mailcrier.d/hook.url\"\n",
			want: `:4:1: target "hook": url_file: /etc/mailcrier.d/hook.url: file does not exist`,
		},
		{
			name: "wrong-value-type",
			doc:  "[target.tg]\ntype = \"telegram\"\ntoken = \"1:a\"\nmessage_thread_id = \"forty-two\"\n",
			want: `:4:21: toml: cannot decode TOML string into struct field config.Target.MessageThreadID of type int64`,
		},
		{
			name: "syntax-error",
			doc:  "[target.hook\n",
			want: `:1:13: toml: expected ']' to close table name`,
		},
		{
			name: "empty-url",
			doc:  "[target.hook]\ntype = \"http\"\npreset = \"generic-json\"\nurl = \"\"\n",
			want: `:4:1: target "hook": value of key "url" is not an absolute http or https URL`,
		},
		{
			name: "url-without-scheme",
			doc:  "[target.hook]\ntype = \"http\"\npreset = \"generic-json\"\nurl = \"hooks.example.org/in/SECRET\"\n",
			want: `:4:1: target "hook": value of key "url" is not an absolute http or https URL`,
		},
		{
			name: "url-with-other-scheme",
			doc:  "[target.hook]\ntype = \"http\"\npreset = \"generic-json\"\nurl = \"ftp://example.org/in\"\n",
			want: `:4:1: target "hook": value of key "url" is not an absolute http or https URL`,
		},
		{
			name: "shoutrrr-url-without-scheme",
			doc:  "[target.bus]\ntype = \"shoutrrr\"\nurl = \"123:abc@telegram\"\n",
			want: `:3:1: target "bus": value of key "url" is not a shoutrrr service URL`,
		},
		{
			name: "discord-url-with-shoutrrr-scheme",
			doc:  "[target.dc]\ntype = \"discord\"\nurl = \"discord://token@channel\"\n",
			want: `:3:1: target "dc": value of key "url" is not an absolute http or https URL`,
		},
		{
			name: "chat-id-missing",
			doc:  "[target.tg]\ntype = \"telegram\"\ntoken = \"1:a\"\n",
			want: `:1:9: target "tg": key "chat_id" is required`,
		},
		{
			name: "chat-id-empty",
			doc:  "[target.tg]\ntype = \"telegram\"\ntoken = \"1:a\"\nchat_id = \"\"\n",
			want: `:4:1: target "tg": value of key "chat_id" must be an integer or a string that is not blank`,
		},
		{
			name: "chat-id-blank",
			doc:  "[target.tg]\ntype = \"telegram\"\ntoken = \"1:a\"\nchat_id = \"  \"\n",
			want: `:4:1: target "tg": value of key "chat_id" must be an integer or a string that is not blank`,
		},
		{
			name: "chat-id-float",
			doc:  "[target.tg]\ntype = \"telegram\"\ntoken = \"1:a\"\nchat_id = 1.5\n",
			want: `:4:1: target "tg": value of key "chat_id" must be an integer or a string that is not blank`,
		},
		{
			name: "channel-blank",
			doc:  "[target.sl]\ntype = \"slack\"\ntoken = \"xoxb-1\"\nchannel = \" \"\n",
			want: `:4:1: target "sl": value of key "channel" is empty`,
		},
		{
			name: "channel-blank-mattermost",
			doc:  "[target.mm]\ntype = \"http\"\npreset = \"mattermost\"\nurl = \"https://mm.example.org/hooks/x\"\nchannel = \"  \"\n",
			want: `:5:1: target "mm": value of key "channel" is empty`,
		},
		{
			name: "channel-blank-without-mattermost",
			doc:  "[target.api]\ntype = \"http\"\npreset = \"generic-json\"\nurl = \"https://api.example.org/notify\"\nchannel = \" \"\n",
			want: `:5:1: target "api": key "channel" needs preset "mattermost"`,
		},
		{
			name: "channel-missing",
			doc:  "[target.sl]\ntype = \"slack\"\ntoken = \"xoxb-1\"\n",
			want: `:1:9: target "sl": key "channel" is required`,
		},
		{
			name: "blank-token",
			doc:  "[target.tg]\ntype = \"telegram\"\ntoken = \"   \"\nchat_id = \"1\"\n",
			want: `:3:1: target "tg": value of key "token" is empty`,
		},
		{
			name: "empty-token",
			doc:  "[target.tg]\ntype = \"telegram\"\ntoken = \"\"\nchat_id = \"1\"\n",
			want: `:3:1: target "tg": value of key "token" is empty`,
		},
		{
			name: "bad-duration",
			doc:  "[general]\nhttp_timeout = \"15\"\n",
			want: `:2:16: toml: invalid duration, want a Go duration such as "15s"`,
		},
		{
			name: "zero-deadline",
			doc:  "[general]\ndeadline = \"0s\"\n\n[target.hook]\ntype = \"http\"\npreset = \"generic-json\"\nurl = \"https://example.org\"\n",
			want: `:2:1: value of key "deadline" must be positive`,
		},
		{
			name: "max-text-zero",
			doc:  "[target.dc]\ntype = \"discord\"\nurl = \"https://example.org/x\"\nmax_text = 0\n",
			want: `:4:1: target "dc": value of key "max_text" must be positive`,
		},
		{
			name: "max-lines-negative",
			doc:  "[target.api]\ntype = \"http\"\npreset = \"generic-json\"\nurl = \"https://example.org/x\"\nmax_lines = -1\n",
			want: `:5:1: target "api": value of key "max_lines" must be positive`,
		},
		{
			name: "max-file-size-zero",
			doc:  "[target.nt]\ntype = \"ntfy\"\nurl = \"https://ntfy.example.org/x\"\nmax_file_size = 0\n",
			want: `:4:1: target "nt": value of key "max_file_size" must be positive`,
		},
		{
			name: "max-file-size-not-integer",
			doc:  "[target.nt]\ntype = \"ntfy\"\nurl = \"https://ntfy.example.org/x\"\nmax_file_size = \"2M\"\n",
			want: `:4:17: toml: cannot decode TOML string into struct field config.Target.MaxFileSize of type int64`,
		},
		{
			name: "max-file-size-of-http",
			doc:  "[target.api]\ntype = \"http\"\npreset = \"generic-json\"\nurl = \"https://example.org/x\"\nmax_file_size = 1000\n",
			want: `:5:1: target "api": key "max_file_size" is not valid for type "http"`,
		},
		{
			name: "unknown-long-file",
			doc:  "[target.nt]\ntype = \"ntfy\"\nurl = \"https://ntfy.example.org/x\"\nlong_file = \"pdf\"\n",
			want: `:4:1: target "nt": unknown file, want one of text, eml`,
		},
		{
			name: "long-file-of-http",
			doc:  "[target.api]\ntype = \"http\"\npreset = \"generic-json\"\nurl = \"https://example.org/x\"\nlong_file = \"eml\"\n",
			want: `:5:1: target "api": key "long_file" is not valid for type "http"`,
		},
		{
			name: "no-targets",
			doc:  "[general]\nsyslog_tag = \"x\"\n",
			want: `: no targets configured`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := load(t, tc.doc, nil)
			var cfgErr *Error
			if !errors.As(err, &cfgErr) {
				t.Fatalf("Load() error = %v (%T), want *Error", err, err)
			}
			if got, want := err.Error(), "/"+configPath+tc.want; got != want {
				t.Errorf("Load() error\n got  %s\n want %s", got, want)
			}
		})
	}
}

// TestLoadErrorOmitsValues pins that a rejected file does not leak a secret
// through the error text, which goes to syslog.
func TestLoadErrorOmitsValues(t *testing.T) {
	const secret = "123456:SECRET-TOKEN"
	docs := []string{
		"[target.tg]\ntype = \"telegram\"\ntoken = \"" + secret + "\"\ntoken_file = \"/x\"\n",
		"[target.tg]\ntype = \"telegram\"\ntoken = \"" + secret + "\"\nbogus = 1\n",
		"[target.tg]\ntype = \"telegram\"\ntoken = \"" + secret + "\"\nchat_id = \n",
		"[general]\nhttp_timeout = \"" + secret + "\"\n",
	}
	for _, doc := range docs {
		_, err := load(t, doc, nil)
		if err == nil {
			t.Fatalf("Load(%q) succeeded", doc)
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error text contains the token: %v", err)
		}
	}
}

func TestLoadRejectsBadURLFileContent(t *testing.T) {
	doc := "[target.hook]\ntype = \"http\"\npreset = \"generic-json\"\nurl_file = \"/etc/mailcrier.d/hook.url\"\n"
	for _, content := range []string{"", "\n", "hooks.example.org/SECRET\n"} {
		_, err := load(t, doc, fstest.MapFS{"etc/mailcrier.d/hook.url": {Data: []byte(content)}})
		want := "/" + configPath + `:4:1: target "hook": value of key "url_file" is not an absolute http or https URL`
		if err == nil || err.Error() != want {
			t.Errorf("url_file content %q: error %v, want %s", content, err, want)
		}
	}
}

// TestLoadTemplates pins that every type but exec takes a template, from
// the file itself or from template_file, and that an http target with a
// template needs no preset.
func TestLoadTemplates(t *testing.T) {
	doc := "[target.hook]\ntype = \"http\"\nurl = \"https://example.org\"\ntemplate = \"{{ toJson .Subject }}\"\n" +
		"[target.tg]\ntype = \"telegram\"\ntoken = \"1:a\"\nchat_id = 1\ntemplate_file = \"/etc/mailcrier.d/tg.tmpl\"\n" +
		"[target.bus]\ntype = \"shoutrrr\"\nurl = \"gotify://example.org/token\"\ntemplate = \"{{ .Body }}\"\n"
	cfg, err := load(t, doc, fstest.MapFS{"etc/mailcrier.d/tg.tmpl": {Data: []byte("<b>{{ .Subject | tgHTML }}</b>\n\n")}})
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"hook": "{{ toJson .Subject }}", "tg": "<b>{{ .Subject | tgHTML }}</b>\n", "bus": "{{ .Body }}"} {
		if got := cfg.Targets[name].Template; got != want {
			t.Errorf("%s: Template = %q, want %q", name, got, want)
		}
	}
	if cfg.Targets["hook"].Preset != "" {
		t.Errorf("Preset = %q", cfg.Targets["hook"].Preset)
	}
	_, err = load(t, "[target.tg]\ntype = \"telegram\"\ntoken = \"1:a\"\nchat_id = 1\ntemplate_file = \"/t.tmpl\"\n", fstest.MapFS{"t.tmpl": {Data: []byte("\n")}})
	if want := "/" + configPath + `:5:1: target "tg": file of key "template_file" is blank`; err == nil || err.Error() != want {
		t.Errorf("blank template_file: %v, want %s", err, want)
	}
}

// TestLoadRequestParts pins the keys of the parts of an http request: a
// GET needs neither preset nor template, path and query values are kept
// as written.
func TestLoadRequestParts(t *testing.T) {
	doc := "[target.hook]\ntype = \"http\"\nurl = \"https://example.org/in\"\nmethod = \"GET\"\npath = \"/h/{{ pathSegment .Hostname }}\"\n" +
		"[target.hook.query]\nsubject = \"{{ .Subject }}\"\nempty = \"\"\n[target.hook.headers]\nX-Host = \"{{ .Hostname }}\"\n"
	cfg, err := load(t, doc, nil)
	if err != nil {
		t.Fatal(err)
	}
	target := cfg.Targets["hook"]
	if target.Path != "/h/{{ pathSegment .Hostname }}" || target.Query["subject"] != "{{ .Subject }}" || target.Query["empty"] != "" || target.Headers["X-Host"] != "{{ .Hostname }}" {
		t.Errorf("target = %+v", target)
	}
}

// TestLoadPathPrefix pins the paths whose fixed start the file cannot
// judge, or judges fine: the action may complete a segment or write the
// whole path, and an escaped percent sign stays allowed.
func TestLoadPathPrefix(t *testing.T) {
	for _, path := range []string{"{{ .Hostname }}", "/{{ .Hostname }}", "/a/.{{ .Hostname }}", "/a/..{{ .Hostname }}", "/100%25/{{ .Hostname }}", "/hooks/x", "/a/%2{{ .Hostname }}"} {
		doc := "[target.hook]\ntype = \"http\"\nurl = \"https://example.org\"\nmethod = \"GET\"\npath = '" + path + "'\n"
		if _, err := load(t, doc, nil); err != nil {
			t.Errorf("path %q: %v", path, err)
		}
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load(fstest.MapFS{}, configPath)
	var cfgErr *Error
	if !errors.As(err, &cfgErr) {
		t.Fatalf("Load() error = %v, want *Error", err)
	}
	if got, want := err.Error(), "/"+configPath+": file does not exist"; got != want {
		t.Errorf("Load() error = %q, want %q, the path once", got, want)
	}
}

// readmeConfigHeading marks the README section whose TOML blocks use the
// current configuration schema.
const readmeConfigHeading = "## Configuration"

var tomlBlock = regexp.MustCompile("(?s)```toml\n(.*?)```")

// TestLoadChatID pins that chat_id takes the integer form of the Bot API
// examples as well as a string, and yields the same chat.
func TestLoadChatID(t *testing.T) {
	for _, value := range []string{"-1001234567890", "\"-1001234567890\"", "\" -1001234567890 \""} {
		cfg, err := load(t, "[target.tg]\ntype = \"telegram\"\ntoken = \"1:a\"\nchat_id = "+value+"\n", nil)
		if err != nil || cfg.Targets["tg"].ChatID != "-1001234567890" {
			t.Errorf("chat_id = %s: err %v, ChatID %q", value, err, cfg.Targets["tg"].ChatID)
		}
	}
	cfg, err := load(t, "[target.tg]\ntype = \"telegram\"\ntoken = \"1:a\"\nchat_id = \"@ops\"\n", nil)
	if err != nil || cfg.Targets["tg"].ChatID != "@ops" {
		t.Errorf("chat_id = @ops: err %v", err)
	}
}

// TestLoadTrimsChannel pins that the channel goes to Slack as it was
// checked, without surrounding white space.
func TestLoadTrimsChannel(t *testing.T) {
	cfg, err := load(t, "[target.sl]\ntype = \"slack\"\ntoken = \"xoxb-1\"\nchannel = \" #alerts \"\n", nil)
	if err != nil || cfg.Targets["sl"].Channel != "#alerts" {
		t.Errorf("err %v, Channel %q", err, cfg.Targets["sl"].Channel)
	}
}

func TestReadmeExamplesLoad(t *testing.T) {
	readme, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	section := sectionAfter(string(readme), readmeConfigHeading)
	blocks := tomlBlock.FindAllStringSubmatch(section, -1)
	if len(blocks) == 0 {
		t.Fatalf("README section %q has no toml block", readmeConfigHeading)
	}
	secrets := fstest.MapFS{}
	for _, name := range []string{"etc/mailcrier.d/hook.url", "etc/mailcrier.d/tg.token", "etc/mailcrier.d/discord.url", "etc/mailcrier.d/slack.token", "etc/mailcrier.d/ntfy.url", "etc/mailcrier.d/mm.url", "etc/mailcrier.d/bus.url"} {
		secrets[name] = &fstest.MapFile{Data: []byte("https://hooks.example.org/in/secret\n")}
	}
	for i, block := range blocks {
		if _, err := load(t, block[1], secrets); err != nil {
			t.Errorf("README toml block %d: %v", i+1, err)
		}
	}
}

// sectionAfter returns the text from the heading line to the next heading of
// the same level.
func sectionAfter(doc, heading string) string {
	start := strings.Index(doc, "\n"+heading+"\n")
	if start < 0 {
		return ""
	}
	rest := doc[start+len(heading)+2:]
	if end := strings.Index(rest, "\n## "); end >= 0 {
		return rest[:end]
	}
	return rest
}

func TestLoadTimeLimits(t *testing.T) {
	t.Run("T-ADJ-51/defaults", func(t *testing.T) {
		cfg, err := load(t, "[target.hook]\ntype = \"http\"\npreset = \"generic-json\"\nurl = \"https://example.org\"\n", nil)
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if cfg.General.HTTPTimeout.Duration != 15*time.Second || cfg.General.Deadline.Duration != 30*time.Second {
			t.Errorf("http_timeout = %v, deadline = %v, want 15s and 30s", cfg.General.HTTPTimeout, cfg.General.Deadline)
		}
	})
	t.Run("configured", func(t *testing.T) {
		doc := "[general]\nhttp_timeout = \"2s\"\ndeadline = \"1m\"\n\n[target.hook]\ntype = \"http\"\npreset = \"generic-json\"\nurl = \"https://example.org\"\n"
		cfg, err := load(t, doc, nil)
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if cfg.General.HTTPTimeout.Duration != 2*time.Second || cfg.General.Deadline.Duration != time.Minute {
			t.Errorf("http_timeout = %v, deadline = %v, want 2s and 1m", cfg.General.HTTPTimeout, cfg.General.Deadline)
		}
	})
}

func TestLoadSpool(t *testing.T) {
	const target = "\n[target.hook]\ntype = \"http\"\npreset = \"generic-json\"\nurl = \"https://example.org\"\n"
	t.Run("T-ADJ-23/defaults", func(t *testing.T) {
		cfg, err := load(t, target, nil)
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		want := Spool{
			Dir: "/var/spool/mailcrier", DrainBudget: Duration{10 * time.Second}, DrainMaxMessages: 20,
			RunBudget: Duration{time.Minute}, QueueTTL: Duration{7 * 24 * time.Hour}, HoldTTL: Duration{7 * 24 * time.Hour},
			FailedTTL: Duration{30 * 24 * time.Hour}, MaxMessages: 1000, MaxBytes: 256 << 20,
			MaxMessagesPerUID: 200, MaxBytesPerUID: 64 << 20,
		}
		if cfg.Spool != want {
			t.Errorf("Spool = %+v, want %+v", cfg.Spool, want)
		}
	})
	t.Run("configured", func(t *testing.T) {
		doc := "[spool]\ndir = \"/srv/spool\"\ndrain_budget = \"5s\"\ndrain_max_messages = 3\nrun_budget = \"2m\"\n" +
			"queue_ttl = \"48h\"\nhold_ttl = \"24h\"\nfailed_ttl = \"72h\"\nmax_queue_messages = 10\nmax_queue_bytes = 1000\n" +
			"max_queue_messages_per_uid = 2\nmax_queue_bytes_per_uid = 100\n" + target
		cfg, err := load(t, doc, nil)
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		want := Spool{
			Dir: "/srv/spool", HasDir: true, DrainBudget: Duration{5 * time.Second}, DrainMaxMessages: 3,
			RunBudget: Duration{2 * time.Minute}, QueueTTL: Duration{48 * time.Hour}, HoldTTL: Duration{24 * time.Hour},
			FailedTTL: Duration{72 * time.Hour}, MaxMessages: 10, MaxBytes: 1000, MaxMessagesPerUID: 2, MaxBytesPerUID: 100,
		}
		if cfg.Spool != want {
			t.Errorf("Spool = %+v, want %+v", cfg.Spool, want)
		}
	})
	t.Run("off", func(t *testing.T) {
		cfg, err := load(t, "[spool]\ndir = \"\"\n"+target, nil)
		if err != nil || cfg.Spool.Dir != "" || !cfg.Spool.HasDir {
			t.Errorf("Spool = %+v, err = %v, want an empty dir that is set", cfg.Spool, err)
		}
	})
	for name, tc := range map[string]struct{ doc, want string }{
		"relative-dir":      {"[spool]\ndir = \"spool\"\n", `:2:1: value of key "dir" must be an absolute path or empty`},
		"zero-budget":       {"[spool]\ndrain_budget = \"0s\"\n", `:2:1: value of key "drain_budget" must be positive`},
		"negative-limit":    {"[spool]\nmax_queue_bytes = -1\n", `:2:1: value of key "max_queue_bytes" must be positive`},
		"zero-max-messages": {"[spool]\ndrain_max_messages = 0\n", `:2:1: value of key "drain_max_messages" must be positive`},
		"unknown-key":       {"[spool]\nttl = \"1h\"\n", `:2:1: unknown key "spool.ttl"`},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := load(t, tc.doc+target, nil)
			if err == nil || err.Error() != "/"+configPath+tc.want {
				t.Errorf("Load() error = %v, want %s", err, "/"+configPath+tc.want)
			}
		})
	}
}

func TestLoadStrings(t *testing.T) {
	const target = "\n[target.hook]\ntype = \"http\"\npreset = \"generic-json\"\nurl = \"https://example.org\"\n"
	t.Run("configured", func(t *testing.T) {
		cfg, err := load(t, "[strings]\nno_subject = \"(ohne Betreff)\"\nempty_body = \"(leer)\"\ntruncated = \"[gekürzt]\"\ntruncated_size = \"[gekürzt, %s]\"\nmore_attachments = \"und %d weitere\"\nnot_sent = \"[nicht gesendet]\"\n"+target, nil)
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if want := (Strings{NoSubject: "(ohne Betreff)", EmptyBody: "(leer)", Truncated: "[gekürzt]", TruncatedSize: "[gekürzt, %s]", MoreAttachments: "und %d weitere", NotSent: "[nicht gesendet]"}); cfg.Strings != want {
			t.Errorf("Strings = %+v, want %+v", cfg.Strings, want)
		}
	})
	t.Run("absent", func(t *testing.T) {
		cfg, err := load(t, target, nil)
		if err != nil || cfg.Strings != (Strings{}) {
			t.Errorf("Strings = %+v, err = %v", cfg.Strings, err)
		}
	})
	for name, doc := range map[string]string{
		"blank":                       "[strings]\nempty_body = \"  \"\n",
		"unknown":                     "[strings]\nsubject = \"x\"\n",
		"more-attachments-no-verb":    "[strings]\nmore_attachments = \"and more\"\n",
		"more-attachments-two-verbs":  "[strings]\nmore_attachments = \"%d of %d\"\n",
		"more-attachments-other-verb": "[strings]\nmore_attachments = \"%s more\"\n",
		"more-attachments-percent":    "[strings]\nmore_attachments = \"%d more, 100%\"\n",
		"truncated-size-no-verb":      "[strings]\ntruncated_size = \"[cut]\"\n",
		"truncated-size-other-verb":   "[strings]\ntruncated_size = \"[cut, %d]\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := load(t, doc+target, nil)
			var cfgErr *Error
			if !errors.As(err, &cfgErr) || cfgErr.Line != 2 {
				t.Errorf("Load() error = %v, want *Error on line 2", err)
			}
		})
	}
}

// TestLoadRules pins that [[route]] and [[suppress]] load with their
// conditions compiled: globs whole and case folded, expressions as
// written.
func TestLoadRules(t *testing.T) {
	doc := twoHTTPTargets + `
[[suppress]]
subject = "*Anacron*"

[[route]]
subject = "*raid*"
targets = ["a"]
continue = true

[[route]]
recipient_regex = '^backup(@|$)'
sender = "root@*"
targets = ["b", "a"]
`
	cfg, err := load(t, doc, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Routes) != 2 || len(cfg.Suppressions) != 1 {
		t.Fatalf("routes %d, suppressions %d", len(cfg.Routes), len(cfg.Suppressions))
	}
	first, second := cfg.Routes[0].Match, cfg.Routes[1].Match
	switch {
	case !cfg.Suppressions[0].Match.Subject.MatchString("ANACRON job"):
		t.Error("suppress glob does not ignore case")
	case !first.Subject.MatchString("md0: RAID\ndegraded"):
		t.Error("route glob does not match across lines")
	case !cfg.Routes[0].Continue || cfg.Routes[1].Continue:
		t.Error("continue not loaded")
	case !second.Recipient.MatchString("backup") || second.Recipient.MatchString("BACKUP"):
		t.Error("regex not taken as written")
	case !second.Sender.MatchString("ROOT@host") || second.Subject != nil || first.Recipient != nil:
		t.Error("conditions misplaced")
	case strings.Join(cfg.Routes[1].Targets, ",") != "b,a":
		t.Errorf("targets %v", cfg.Routes[1].Targets)
	}
}

// twoHTTPTargets declares targets a and b.
const twoHTTPTargets = "[target.a]\ntype = \"http\"\npreset = \"generic-json\"\nurl = \"https://example.org/a\"\n\n" +
	"[target.b]\ntype = \"http\"\npreset = \"generic-json\"\nurl = \"https://example.org/b\"\n"

// TestLoadRejectsRules pins the errors of [[route]] and [[suppress]]: each
// names the rule by its number and points at its line, never quoting a
// value.
func TestLoadRejectsRules(t *testing.T) {
	// twoHTTPTargets takes 9 lines; the rules start at line 10.
	cases := []struct {
		name, rules, want string
	}{
		{"unknown-target", "[[route]]\ntargets = [\"a\", \"nosuch\"]\n", `:11:1: route 1: element 2 of key "targets" is not a configured target`},
		{"repeated-target", "[[route]]\ntargets = [\"a\", \"a\"]\n", `:11:1: route 1: element 2 of key "targets" repeats a target`},
		{"empty-targets", "[[route]]\ntargets = []\n", `:11:1: route 1: key "targets" must name at least one target`},
		{"missing-targets", "[[route]]\nsubject = \"x\"\n", `:10:3: route 1: key "targets" must name at least one target`},
		{"glob-and-regex", "[[route]]\nsubject = \"x\"\nsubject_regex = \"x\"\ntargets = [\"a\"]\n", `:12:1: route 1: keys "subject" and "subject_regex" are mutually exclusive`},
		{"empty-glob", "[[route]]\nrecipient = \"\"\ntargets = [\"a\"]\n", `:11:1: route 1: value of key "recipient" must not be empty`},
		{"empty-regex", "[[suppress]]\nsender_regex = ''\n", `:11:1: suppress 1: value of key "sender_regex" must not be empty`},
		{"trailing-backslash", "[[suppress]]\nsubject = 'x\\'\n", `:11:1: suppress 1: value of key "subject" ends with a backslash that escapes nothing`},
		{"bad-regex", "[[route]]\nsubject_regex = '(MARKER'\ntargets = [\"a\"]\n", `:11:1: route 1: value of key "subject_regex" is not a valid expression: missing closing )`},
		{"suppress-without-condition", "[[suppress]]\n", `:10:3: suppress 1: a condition is required, or every message is suppressed`},
		{"recipient-in-suppress", "[[suppress]]\nrecipient = \"root\"\n", `:11:1: unknown key "suppress.recipient"`},
		{"third-rule", "[[route]]\ntargets = [\"a\"]\n[[route]]\ntargets = [\"b\"]\n\n[[route]]\nsubject = \"x\"\ntargets = [\"c\"]\n", `:17:1: route 3: element 1 of key "targets" is not a configured target`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := load(t, twoHTTPTargets+tc.rules, nil)
			if err == nil {
				t.Fatal("Load() succeeded")
			}
			if got, want := err.Error(), "/"+configPath+tc.want; got != want {
				t.Errorf("Load() error\n got  %s\n want %s", got, want)
			}
			if strings.Contains(err.Error(), "MARKER") {
				t.Errorf("error quotes the expression: %v", err)
			}
		})
	}
}

// telegramBase declares the telegram target tg beside the http target a.
const telegramBase = "[target.tg]\ntype = \"telegram\"\ntoken = \"1:a\"\nchat_id = 1\n\n" +
	"[target.a]\ntype = \"http\"\npreset = \"generic-json\"\nurl = \"https://example.org/a\"\n"

// TestLoadDirectChats pins telegram_direct_chats normalized and the
// default of telegram_direct_max.
func TestLoadDirectChats(t *testing.T) {
	cfg, err := load(t, "[general]\ntelegram_direct = \"tg\"\ntelegram_direct_chats = [1234, -100123, \"@Ops_Channel\"]\n\n"+telegramBase, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cfg.General.TelegramDirectChats, ","); got != "1234,-100123,@ops_channel" || cfg.General.TelegramDirectMax != DefaultTelegramDirectMax {
		t.Errorf("chats %s, max %d", got, cfg.General.TelegramDirectMax)
	}
	cfg, err = load(t, "[general]\ntelegram_direct = \"tg\"\ntelegram_direct_chats = [1]\ntelegram_direct_max = 3\n\n"+telegramBase, nil)
	if err != nil || cfg.General.TelegramDirectMax != 3 {
		t.Errorf("Load() = %v, max %v", err, cfg)
	}
}

// TestLoadRejectsDirectChats pins the errors of the telegram_direct keys.
func TestLoadRejectsDirectChats(t *testing.T) {
	cases := []struct {
		name, general, want string
	}{
		{"not-a-telegram-target", "telegram_direct = \"a\"\ntelegram_direct_chats = [1]\n", `:2:1: value of key "telegram_direct" must name a target of type "telegram"`},
		{"unknown-target", "telegram_direct = \"nosuch\"\ntelegram_direct_chats = [1]\n", `:2:1: value of key "telegram_direct" must name a target of type "telegram"`},
		{"chats-missing", "telegram_direct = \"tg\"\n", `:2:1: key "telegram_direct" needs key "telegram_direct_chats"`},
		{"chats-empty", "telegram_direct = \"tg\"\ntelegram_direct_chats = []\n", `:3:1: value of key "telegram_direct_chats" must not be empty`},
		{"chats-without-direct", "telegram_direct_chats = [1]\n", `:2:1: key "telegram_direct_chats" needs key "telegram_direct"`},
		{"max-without-direct", "telegram_direct_max = 5\n", `:2:1: key "telegram_direct_max" needs key "telegram_direct"`},
		{"max-zero", "telegram_direct = \"tg\"\ntelegram_direct_chats = [1]\ntelegram_direct_max = 0\n", `:4:1: value of key "telegram_direct_max" must be positive`},
		{"chat-string-of-digits", "telegram_direct = \"tg\"\ntelegram_direct_chats = [1, \"1234\"]\n", `:3:1: element 2 of key "telegram_direct_chats" must be an integer or an @username`},
		{"chat-float", "telegram_direct = \"tg\"\ntelegram_direct_chats = [1.5]\n", `:3:1: element 1 of key "telegram_direct_chats" must be an integer or an @username`},
		{"chat-bool", "telegram_direct = \"tg\"\ntelegram_direct_chats = [true]\n", `:3:1: element 1 of key "telegram_direct_chats" must be an integer or an @username`},
		{"chat-array", "telegram_direct = \"tg\"\ntelegram_direct_chats = [[1]]\n", `:3:1: element 1 of key "telegram_direct_chats" must be an integer or an @username`},
		{"chat-short-username", "telegram_direct = \"tg\"\ntelegram_direct_chats = [\"@abc\"]\n", `:3:1: element 1 of key "telegram_direct_chats" must be an integer or an @username`},
		{"chat-repeated", "telegram_direct = \"tg\"\ntelegram_direct_chats = [\"@Ops_Channel\", \"@ops_channel\"]\n", `:3:1: element 2 of key "telegram_direct_chats" repeats a chat`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := load(t, "[general]\n"+tc.general+"\n"+telegramBase, nil)
			if err == nil {
				t.Fatal("Load() succeeded")
			}
			if got, want := err.Error(), "/"+configPath+tc.want; got != want {
				t.Errorf("Load() error\n got  %s\n want %s", got, want)
			}
		})
	}
}

// TestLoadRejectsInlineRules pins the position of an error in rules
// written as an inline array: its elements are not indexed, so the error
// points at the key of the array, with the number of the rule right.
func TestLoadRejectsInlineRules(t *testing.T) {
	doc := "route = [\n  { targets = [\"a\"] },\n  { subject_regex = '(MARKER', targets = [\"a\"] },\n]\n\n" + twoHTTPTargets
	_, err := load(t, doc, nil)
	want := "/" + configPath + `:1:1: route 2: value of key "subject_regex" is not a valid expression: missing closing )`
	if err == nil || err.Error() != want {
		t.Errorf("Load() error\n got  %v\n want %s", err, want)
	}
}
