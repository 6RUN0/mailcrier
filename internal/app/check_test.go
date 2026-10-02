package app

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"testing/fstest"
	"testing/iotest"

	"github.com/6RUN0/slendmail/internal/config"
	"github.com/6RUN0/slendmail/internal/redact"
)

// checkSummaryClean is the stderr of --check-config for a file without
// findings.
const checkSummaryClean = "/etc/slendmail.conf: 0 errors, 0 warnings\n"

// checkConfig runs --check-config as an unelevated caller on doc with
// files beside it.
func checkConfig(t *testing.T, doc string, files fstest.MapFS) (int, *invocation) {
	t.Helper()
	inv := &invocation{config: doc, files: files, args: []string{"--check-config"}, creds: plainUser, stdin: iotest.ErrReader(errors.New("stdin read"))}
	return inv.run(t), inv
}

// linesWith returns the lines of text that start with prefix.
func linesWith(text, prefix string) []string {
	var found []string
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, prefix) {
			found = append(found, line)
		}
	}
	return found
}

// tripwireTransport fails the test on any request.
type tripwireTransport struct{ t *testing.T }

func (tw tripwireTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	tw.t.Errorf("request sent to %s", req.URL.Host)
	return nil, errors.New("no request expected")
}

func TestCheckConfig(t *testing.T) {
	t.Run("T-TPL-15/parse-error", func(t *testing.T) {
		code, inv := checkConfig(t, "[target.api]\ntype = \"http\"\nurl = \"https://example.org/x\"\ntemplate = \"{{ .Subject \"\n", nil)
		if code != 78 {
			t.Fatalf("Run() = %d, want 78; output:\n%s", code, inv.output())
		}
		if errs := linesWith(inv.stderr.String(), "error: "); len(errs) != 1 || !strings.Contains(errs[0], `target "api"`) {
			t.Errorf("stderr = %q, want one error about target \"api\"", inv.stderr.String())
		}
	})
	t.Run("valid", func(t *testing.T) {
		spoolDir := filepath.Join(t.TempDir(), "spool")
		inv := &invocation{config: httpTargetConfig("https://example.org/x"), args: []string{"--check-config"}, spoolDir: spoolDir, stdin: iotest.ErrReader(errors.New("stdin read"))}
		if code := inv.run(t); code != 0 {
			t.Fatalf("Run() = %d, want 0; output:\n%s", code, inv.output())
		}
		if inv.stdout.Len() != 0 || inv.stderr.String() != checkSummaryClean {
			t.Errorf("stdout = %q, stderr = %q, want nothing and %q", inv.stdout.String(), inv.stderr.String(), checkSummaryClean)
		}
		if _, err := os.Stat(spoolDir); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("spool directory: %v, want it not created", err)
		}
		if log := inv.log("slendmail"); !strings.Contains(log, `level=INFO msg="configuration checked" errors=0 warnings=0`) {
			t.Errorf("log lacks the record:\n%s", log)
		}
	})
	t.Run("warnings-after-target-error", func(t *testing.T) {
		doc := "[target.dc]\ntype = \"discord\"\nurl = \"https://example.org/x\"\ntemplate = \"{{ if }}\"\n[[route]]\nsubject = \"*\"\ntargets = [\"dc\"]\n"
		code, inv := checkConfig(t, doc, nil)
		if code != 78 {
			t.Fatalf("Run() = %d, want 78; output:\n%s", code, inv.output())
		}
		lines := strings.Split(strings.TrimSuffix(inv.stderr.String(), "\n"), "\n")
		if len(lines) != 3 || !strings.HasPrefix(lines[0], `error: /etc/slendmail.conf: target "dc": `) ||
			!strings.HasPrefix(lines[1], "warning: /etc/slendmail.conf:5:3: routes have no rule without conditions") || lines[2] != "/etc/slendmail.conf: 1 error, 1 warning" {
			t.Errorf("stderr:\n%s", inv.stderr.String())
		}
		if log := inv.log("slendmail"); !strings.Contains(log, `level=INFO msg="configuration checked" errors=1 warnings=1`) {
			t.Errorf("log lacks the record:\n%s", log)
		}
	})
	t.Run("token-of-rejected-file-hidden", func(t *testing.T) {
		const token = "123456:SECRET-TOKEN-0123456789"
		for _, doc := range []string{
			"[target.tg]\ntype = \"telegram\"\ntoken = \"" + token + "\"\nchat_id = \n",
			"[target.tg]\ntype = \"telegram\"\ntoken = \"" + token + "\"\nchat_id = \"1\n",
		} {
			code, inv := checkConfig(t, doc, nil)
			if code != 78 {
				t.Fatalf("Run() = %d, want 78", code)
			}
			if strings.Contains(inv.output()+inv.stdout.String(), "SECRET") {
				t.Errorf("token in the output:\n%s", inv.output())
			}
		}
	})
	t.Run("template-of-one-target-fails", func(t *testing.T) {
		doc := "[target.a]\ntype = \"discord\"\nurl = \"https://example.org/a\"\ntemplate = \"{{ .Nope }}\"\n" +
			"[target.b]\ntype = \"discord\"\nurl = \"https://example.org/b\"\ntemplate = \"{{ .Subject }}\"\n"
		code, inv := checkConfig(t, doc, nil)
		if code != 0 {
			t.Fatalf("Run() = %d, want 0; output:\n%s", code, inv.output())
		}
		warnings := linesWith(inv.stderr.String(), "warning: ")
		want := `warning: /etc/slendmail.conf: target "a": template fails on the sample message, the built-in one is used: `
		if len(warnings) != 1 || !strings.HasPrefix(warnings[0], want) {
			t.Errorf("warnings = %q, want one starting with %q", warnings, want)
		}
	})
	t.Run("header-template-fails", func(t *testing.T) {
		doc := "[target.api]\ntype = \"http\"\npreset = \"generic-json\"\nurl = \"https://example.org/x\"\nheaders = { Authorization = \"Bearer SECRETVALUE-0123456789 {{ .Nope }}\" }\n"
		code, inv := checkConfig(t, doc, nil)
		if code != 0 {
			t.Fatalf("Run() = %d, want 0; output:\n%s", code, inv.output())
		}
		warnings := linesWith(inv.stderr.String(), "warning: ")
		if len(warnings) != 1 || !strings.Contains(warnings[0], `target "api": request template fails on the sample message, the target gets nothing: `) {
			t.Errorf("warnings = %q", warnings)
		}
		if strings.Contains(inv.output(), "SECRETVALUE") {
			t.Errorf("header value in the output:\n%s", inv.output())
		}
	})
	t.Run("limit-below-fixed-part", func(t *testing.T) {
		code, inv := checkConfig(t, httpTargetConfig("https://example.org/x")+"max_text = 10\n", nil)
		if code != 0 {
			t.Fatalf("Run() = %d, want 0; output:\n%s", code, inv.output())
		}
		warnings := linesWith(inv.stderr.String(), "warning: ")
		if len(warnings) != 1 || !strings.Contains(warnings[0], `target "hook": the sample message does not render, the target gets nothing: `) {
			t.Errorf("warnings = %q", warnings)
		}
	})
	t.Run("get-without-template", func(t *testing.T) {
		code, inv := checkConfig(t, "[target.ping]\ntype = \"http\"\nmethod = \"GET\"\nurl = \"https://example.org/ping\"\n", nil)
		if code != 0 || inv.stderr.String() != checkSummaryClean {
			t.Errorf("Run() = %d, stderr = %q, want 0 and %q", code, inv.stderr.String(), checkSummaryClean)
		}
	})
	t.Run("sends-nothing", func(t *testing.T) {
		marker := filepath.Join(t.TempDir(), "ran")
		doc := "[target.tg]\ntype = \"telegram\"\ntoken = \"1:a\"\nchat_id = 1\n" +
			"[target.dc]\ntype = \"discord\"\nurl = \"https://example.org/dc\"\n" +
			"[target.sl]\ntype = \"slack\"\ntoken = \"xoxb-1\"\nchannel = \"#ops\"\n" +
			"[target.nt]\ntype = \"ntfy\"\nurl = \"https://example.org/topic\"\n" +
			httpTargetConfig("https://example.org/hook") +
			"[target.run]\ntype = \"exec\"\nargv = [\"" + writeHookScript(t, "touch "+marker+"\n") + "\"]\n" +
			shoutrrrTargetConfig
		inv := &invocation{config: doc, args: []string{"--check-config"}, client: &http.Client{Transport: tripwireTransport{t}}, stdin: iotest.ErrReader(errors.New("stdin read"))}
		if code := inv.run(t); code != 0 {
			t.Fatalf("Run() = %d, want 0; output:\n%s", code, inv.output())
		}
		if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("hook ran: %v", err)
		}
	})
	t.Run("unreadable-by-group", func(t *testing.T) {
		doc := "[target.tg]\ntype = \"telegram\"\ntoken_file = \"/etc/slendmail.d/tg.token\"\nchat_id = 1\n"
		files := fstest.MapFS{"etc/slendmail.d/tg.token": {Data: []byte("1:a\n"), Mode: 0o640, Sys: &syscall.Stat_t{Gid: 0}}}
		want := `warning: /etc/slendmail.conf:3:1: target "tg": file of key "token_file" is not readable by the group of the binary, every call of another user exits 78`
		for _, tc := range []struct {
			name  string
			creds Credentials
			want  []string
		}{{"elevated", elevatedRoot, []string{want}}, {"unelevated", plainUser, nil}} {
			inv := &invocation{config: doc, files: files, args: []string{"--check-config"}, creds: tc.creds, stdin: iotest.ErrReader(errors.New("stdin read"))}
			if code := inv.run(t); code != 0 {
				t.Fatalf("%s: Run() = %d, want 0; output:\n%s", tc.name, code, inv.output())
			}
			if got := linesWith(inv.stderr.String(), "warning: "); strings.Join(got, "\n") != strings.Join(tc.want, "\n") {
				t.Errorf("%s: warnings = %q, want %q", tc.name, got, tc.want)
			}
		}
	})
	t.Run("caller-not-privileged", func(t *testing.T) {
		inv := &invocation{config: httpTargetConfig("https://example.org/x"), args: []string{"--check-config"}, creds: elevatedUser, stdin: iotest.ErrReader(errors.New("stdin read"))}
		if code := inv.run(t); code != 77 {
			t.Fatalf("Run() = %d, want 77", code)
		}
		if inv.stdout.Len() != 0 || inv.stderr.String() != "slendmail: --check-config: permission denied\n" {
			t.Errorf("stdout = %q, stderr = %q", inv.stdout.String(), inv.stderr.String())
		}
	})
	t.Run("arguments", func(t *testing.T) {
		inv := &invocation{config: httpTargetConfig("https://example.org/x"), args: []string{"--check-config", "root"}, stdin: iotest.ErrReader(errors.New("stdin read"))}
		if code := inv.run(t); code != 64 {
			t.Fatalf("Run() = %d, want 64", code)
		}
		if got := inv.stderr.String(); got != "slendmail: --check-config takes no arguments\n" {
			t.Errorf("stderr = %q", got)
		}
	})
}

// checkConfigCase is one item of TestCheckConfigRoadmapItems: the exit
// status and the one error, or with 0 the one warning, --check-config
// prints for doc. Warnings of a file that loads come with an error of its
// targets as well.
type checkConfigCase struct {
	doc   string
	files fstest.MapFS
	args  []string
	creds Credentials
	want  int
	// line is part of the finding.
	line string
}

func (tc checkConfigCase) check(t *testing.T) {
	t.Helper()
	args := tc.args
	if args == nil {
		args = []string{"--check-config"}
	}
	inv := &invocation{config: tc.doc, files: tc.files, args: args, creds: tc.creds, stdin: iotest.ErrReader(errors.New("stdin read"))}
	if code := inv.run(t); code != tc.want {
		t.Fatalf("Run() = %d, want %d; output:\n%s", code, tc.want, inv.output())
	}
	kind := map[int]string{0: "warning: ", 78: "error: "}[tc.want]
	stderr := inv.stderr.String()
	if found := linesWith(stderr, kind); len(found) != 1 || !strings.Contains(found[0], tc.line) {
		t.Errorf("stderr:\n%s\nwant one %q line with %q", stderr, kind, tc.line)
	}
	if tc.want == 0 && len(linesWith(stderr, "error: ")) != 0 {
		t.Errorf("stderr:\n%s\nwant no error", stderr)
	}
}

// TestCheckConfigRoadmapItems runs --check-config on one defect each: what
// every call checks, through the mode, and the warnings only the mode
// gives.
func TestCheckConfigRoadmapItems(t *testing.T) {
	const tg = "[target.tg]\ntype = \"telegram\"\ntoken = \"1:a\"\nchat_id = 1\n"
	cases := []struct {
		name string
		checkConfigCase
	}{
		{"file-missing", checkConfigCase{doc: tg, args: []string{"--check-config", "--config", "/etc/missing.conf"}, want: 78,
			line: "error: /etc/missing.conf: file does not exist"}},
		{"unknown-key", checkConfigCase{doc: tg + "bogus = 1\n", want: 78, line: `/etc/slendmail.conf:5:1: unknown key "target.tg.bogus"`}},
		{"key-of-other-type", checkConfigCase{doc: tg + "channel = \"#x\"\n", want: 78, line: `/etc/slendmail.conf:5:1: target "tg": key "channel" is not valid for type "telegram"`}},
		{"no-targets", checkConfigCase{doc: "[general]\nsyslog_tag = \"x\"\n", want: 78, line: "/etc/slendmail.conf: no targets configured"}},
		{"route-to-unknown-target", checkConfigCase{doc: tg + "[[route]]\ntargets = [\"nope\"]\n", want: 78,
			line: `/etc/slendmail.conf:6:1: route 1: element 1 of key "targets" is not a configured target`}},
		{"glob-ends-in-backslash", checkConfigCase{doc: tg + "[[route]]\nsubject = \"x\\\\\"\ntargets = [\"tg\"]\n", want: 78, line: `/etc/slendmail.conf:6:1: route 1: value of key "subject"`}},
		{"invalid-regex", checkConfigCase{doc: tg + "[[route]]\nsubject_regex = \"(\"\ntargets = [\"tg\"]\n", want: 78,
			line: `/etc/slendmail.conf:6:1: route 1: value of key "subject_regex" is not a valid expression: missing closing )`}},
		{"token-file-missing", checkConfigCase{doc: "[target.tg]\ntype = \"telegram\"\ntoken_file = \"/etc/slendmail.d/tg.token\"\nchat_id = 1\n", want: 78,
			line: `/etc/slendmail.conf:3:1: target "tg": token_file: /etc/slendmail.d/tg.token: file does not exist`}},
		{"url-not-http", checkConfigCase{doc: "[target.dc]\ntype = \"discord\"\nurl = \"ftp://example.org/x\"\n", want: 78,
			line: `/etc/slendmail.conf:3:1: target "dc": value of key "url" is not an absolute http or https URL`}},
		{"template-parse-error", checkConfigCase{doc: "[target.dc]\ntype = \"discord\"\nurl = \"https://example.org/x\"\ntemplate = \"{{ if }}\"\n", want: 78,
			line: `error: /etc/slendmail.conf: target "dc": `}},
		{"slack-webhook-on-discord", checkConfigCase{doc: "[target.x]\ntype = \"http\"\nurl = \"https://discord.com/api/webhooks/1/a/slack\"\npreset = \"slack-webhook\"\n",
			line: `/etc/slendmail.conf:4:1: target "x": preset "slack-webhook" on a Discord host does not disable mentions`}},
		{"routes-all-with-conditions", checkConfigCase{doc: tg + "[[route]]\nsubject = \"*\"\ntargets = [\"tg\"]\n", line: `/etc/slendmail.conf:5:3: routes have no rule without conditions`}},
		{"target-without-route", checkConfigCase{doc: tg + "[target.dc]\ntype = \"discord\"\nurl = \"https://example.org/x\"\n[[route]]\ntargets = [\"tg\"]\n",
			line: `/etc/slendmail.conf:5:9: target "dc": no route names this target`}},
		{"secret-file-readable-by-all", checkConfigCase{doc: "[target.tg]\ntype = \"telegram\"\ntoken_file = \"/etc/slendmail.d/tg.token\"\nchat_id = 1\n",
			files: fstest.MapFS{"etc/slendmail.d/tg.token": {Data: []byte("1:a\n"), Mode: 0o644}}, creds: elevatedRoot,
			line: `/etc/slendmail.conf:3:1: target "tg": file of key "token_file" is readable by all users`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, tc.check)
	}
}

// TestRegisterSecretsCoversSecretKeys pins that every key config.Check
// counts as secret is masked in the log: a marker in the value of each key
// is registered.
func TestRegisterSecretsCoversSecretKeys(t *testing.T) {
	const marker = "MARKER0123456789abcdef"
	docs := map[string]string{
		"token":   "[target.tg]\ntype = \"telegram\"\ntoken = \"" + marker + "\"\nchat_id = 1\n",
		"url":     "[target.dc]\ntype = \"discord\"\nurl = \"https://example.org/api/" + marker + "\"\n",
		"headers": "[target.api]\ntype = \"http\"\npreset = \"generic-json\"\nurl = \"https://example.org/x\"\nheaders = { Authorization = \"Bearer " + marker + "\" }\n",
		"query":   "[target.api]\ntype = \"http\"\npreset = \"generic-json\"\nurl = \"https://example.org/x\"\nquery = { key = \"" + marker + "\" }\n",
		"path":    "[target.api]\ntype = \"http\"\npreset = \"generic-json\"\nurl = \"https://example.org/x\"\npath = \"/" + marker + "/{{ .Subject | pathSegment }}\"\n",
	}
	for _, key := range config.SecretKeys {
		doc, ok := docs[key]
		if !ok {
			t.Errorf("no case for secret key %q", key)
			continue
		}
		cfg, err := config.Load(fstest.MapFS{SystemConfigPath: {Data: []byte(doc)}}, SystemConfigPath)
		if err != nil {
			t.Fatalf("key %q: %v", key, err)
		}
		redactor := &redact.Redactor{}
		registerSecrets(redactor, cfg)
		if got := redactor.String(marker); got != redact.Mask {
			t.Errorf("key %q: marker logged as %q", key, got)
		}
	}
}
