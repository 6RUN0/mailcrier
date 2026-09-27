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

const configPath = "etc/slendmail.conf"

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
url_file = "/etc/slendmail.d/hook.url"
preset = "generic-json"

[target.ops-telegram]
type = "telegram"
token = "123:abc"
chat_id = "-100123"
on_long = "file"
`
	secret := fstest.MapFS{"etc/slendmail.d/hook.url": {Data: []byte("https://example.org/hooks/secret\n")}}
	cfg, err := load(t, doc, secret)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.General.SyslogTag != "mail-notify" {
		t.Errorf("SyslogTag = %q", cfg.General.SyslogTag)
	}
	hook := cfg.Targets["hook"]
	if hook.Type != TypeHTTP || hook.Preset != PresetGenericJSON {
		t.Errorf("hook = %+v", hook)
	}
	if hook.URL != "https://example.org/hooks/secret" {
		t.Errorf("hook.URL = %q, want the trimmed content of url_file", hook.URL)
	}
	if tg := cfg.Targets["ops-telegram"]; tg.Token != "123:abc" || tg.ChatID != "-100123" || tg.OnLong != OnLongFile {
		t.Errorf("ops-telegram = %+v", tg)
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
	cfg, err := load(t, "[target.hook]\ntype = \"http\"\nurl = \"https://example.org\"\n", nil)
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
			doc:  "[target.hook]\ntype = \"http\"\nurl = \"https://example.org\"\ncolour = \"red\"\n",
			want: `:4:1: unknown key "target.hook.colour"`,
		},
		{
			name: "unknown-top-level-table",
			doc:  "[target.hook]\ntype = \"http\"\nurl = \"https://example.org\"\n\n[spool]\ndir = \"/var/spool/slendmail\"\n",
			want: `:5:2: unknown key "spool"`,
		},
		{
			name: "key-of-other-target-type",
			doc:  "[target.hook]\ntype = \"http\"\nurl = \"https://example.org\"\nchat_id = \"-100123\"\n",
			want: `:4:1: target "hook": key "chat_id" is not valid for type "http"`,
		},
		{
			name: "http-headers-not-supported",
			doc:  "[target.hook]\ntype = \"http\"\nurl = \"https://example.org\"\n\n[target.hook.headers]\nAuthorization = \"Bearer SECRET\"\n",
			want: `:5:2: unknown key "target.hook.headers"`,
		},
		{
			name: "http-channel-not-supported",
			doc:  "[target.hook]\ntype = \"http\"\nurl = \"https://example.org\"\nchannel = \"alerts\"\n",
			want: `:4:1: target "hook": key "channel" is not valid for type "http"`,
		},
		{
			name: "invalid-name",
			doc:  "[target.Ops_TG]\ntype = \"http\"\nurl = \"https://example.org\"\n",
			want: `:1:9: target "Ops_TG": name must match ^[a-z0-9-]+$`,
		},
		{
			name: "token-and-token-file",
			doc:  "[target.tg]\ntype = \"telegram\"\ntoken = \"123:abc\"\ntoken_file = \"/etc/slendmail.d/tg.token\"\nchat_id = \"1\"\n",
			want: `:4:1: target "tg": keys "token" and "token_file" are mutually exclusive`,
		},
		{
			name: "neither-url-nor-url-file",
			doc:  "\n[target.hook]\ntype = \"http\"\n",
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
			doc:  "[target.hook]\ntype = \"http\"\nurl_file = \"hook.url\"\n",
			want: `:3:1: target "hook": key "url_file" must be an absolute path`,
		},
		{
			name: "missing-secret-file",
			doc:  "[target.hook]\ntype = \"http\"\nurl_file = \"/etc/slendmail.d/hook.url\"\n",
			want: `:3:1: target "hook": url_file: open etc/slendmail.d/hook.url: file does not exist`,
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
			doc:  "[target.hook]\ntype = \"http\"\nurl = \"hooks.example.org/in/SECRET\"\n",
			want: `:3:1: target "hook": value of key "url" is not an absolute http or https URL`,
		},
		{
			name: "url-with-other-scheme",
			doc:  "[target.hook]\ntype = \"http\"\nurl = \"ftp://example.org/in\"\n",
			want: `:3:1: target "hook": value of key "url" is not an absolute http or https URL`,
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
			name: "blank-token",
			doc:  "[target.tg]\ntype = \"telegram\"\ntoken = \"   \"\n",
			want: `:3:1: target "tg": value of key "token" is empty`,
		},
		{
			name: "empty-token",
			doc:  "[target.tg]\ntype = \"telegram\"\ntoken = \"\"\n",
			want: `:3:1: target "tg": value of key "token" is empty`,
		},
		{
			name: "bad-duration",
			doc:  "[general]\nhttp_timeout = \"15\"\n",
			want: `:2:16: toml: invalid duration, want a Go duration such as "15s"`,
		},
		{
			name: "zero-deadline",
			doc:  "[general]\ndeadline = \"0s\"\n\n[target.hook]\ntype = \"http\"\nurl = \"https://example.org\"\n",
			want: `:2:1: value of key "deadline" must be positive`,
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
			if got, want := err.Error(), configPath+tc.want; got != want {
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
	doc := "[target.hook]\ntype = \"http\"\nurl_file = \"/etc/slendmail.d/hook.url\"\n"
	for _, content := range []string{"", "\n", "hooks.example.org/SECRET\n"} {
		_, err := load(t, doc, fstest.MapFS{"etc/slendmail.d/hook.url": {Data: []byte(content)}})
		want := configPath + `:3:1: target "hook": value of key "url_file" is not an absolute http or https URL`
		if err == nil || err.Error() != want {
			t.Errorf("url_file content %q: error %v, want %s", content, err, want)
		}
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load(fstest.MapFS{}, configPath)
	var cfgErr *Error
	if !errors.As(err, &cfgErr) {
		t.Fatalf("Load() error = %v, want *Error", err)
	}
}

// readmeConfigHeading marks the README section whose TOML blocks use the
// current configuration schema.
const readmeConfigHeading = "## Configuration (cmd/slendmail)"

var tomlBlock = regexp.MustCompile("(?s)```toml\n(.*?)```")

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
	for _, name := range []string{"etc/slendmail.d/hook.url"} {
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
		cfg, err := load(t, "[target.hook]\ntype = \"http\"\nurl = \"https://example.org\"\n", nil)
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if cfg.General.HTTPTimeout.Duration != 15*time.Second || cfg.General.Deadline.Duration != 30*time.Second {
			t.Errorf("http_timeout = %v, deadline = %v, want 15s and 30s", cfg.General.HTTPTimeout, cfg.General.Deadline)
		}
	})
	t.Run("configured", func(t *testing.T) {
		doc := "[general]\nhttp_timeout = \"2s\"\ndeadline = \"1m\"\n\n[target.hook]\ntype = \"http\"\nurl = \"https://example.org\"\n"
		cfg, err := load(t, doc, nil)
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if cfg.General.HTTPTimeout.Duration != 2*time.Second || cfg.General.Deadline.Duration != time.Minute {
			t.Errorf("http_timeout = %v, deadline = %v, want 2s and 1m", cfg.General.HTTPTimeout, cfg.General.Deadline)
		}
	})
}
