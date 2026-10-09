package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"log/syslog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"testing/iotest"
	"time"

	"github.com/6RUN0/mailcrier/internal/delivery"
	"github.com/6RUN0/mailcrier/internal/message"
	"github.com/6RUN0/mailcrier/internal/render"
	"github.com/6RUN0/mailcrier/internal/spool"
)

// invocation is one Run call against an in-memory configuration with the
// log captured per syslog tag.
type invocation struct {
	config string
	stdin  io.Reader
	client *http.Client
	logs   map[string]*bytes.Buffer
	// stderr collects what the standard log package prints through the
	// writer Run installs with SetLogOutput.
	stderr    bytes.Buffer
	logOutput io.Writer
	// args defaults to cron's "-ti".
	args []string
	// creds, environ and files set the ids, the environment and extra
	// files of the process.
	creds   Credentials
	environ []string
	files   fstest.MapFS
	// configFS replaces config and files when set.
	configFS fs.FS
	// catchSignals is Deps.CatchSignals.
	catchSignals func(ctx context.Context) (context.Context, context.CancelFunc)
	// execs records the Exec calls, which all fail; replacedEnv is the
	// environment passed to ReplaceEnv.
	execs       []execCall
	replacedEnv []string
	// program is argv[0], "/usr/sbin/sendmail" when empty; stdout collects
	// the output of the modes that print.
	program string
	stdout  bytes.Buffer
	// stdoutWriter replaces stdout when set.
	stdoutWriter io.Writer
	// deliver replaces the delivery to the configured targets.
	deliver func(ctx context.Context, targets []delivery.Target, env message.Envelope, d render.Data, files []message.Attachment) []delivery.Result
	// spoolDir is Deps.SpoolDir, empty for no spool; now replaces the
	// clock, testNow when nil.
	spoolDir string
	now      func() time.Time
	// spoolSaved and entryLocked are the spool hooks of Deps.
	spoolSaved  func(e *spool.Entry)
	entryLocked func(id string)
	// ctx is the context of Run, context.Background when nil.
	ctx context.Context
	// isSyslogDown makes the log go to stderr, as without a syslog socket.
	isSyslogDown bool
}

// execCall is one attempt to replace the process image.
type execCall struct {
	path      string
	argv, env []string
}

func (inv *invocation) run(t testing.TB) int {
	t.Helper()
	inv.logs = map[string]*bytes.Buffer{}
	client := inv.client
	if client == nil {
		client = &http.Client{}
	}
	fsys := fstest.MapFS{SystemConfigPath: {Data: []byte(inv.config)}}
	for name, file := range inv.files {
		fsys[name] = file
	}
	args := inv.args
	if args == nil {
		args = []string{"-ti"}
	}
	program := inv.program
	if program == "" {
		program = "/usr/sbin/sendmail"
	}
	// As main does: Harden first, with an Exec that always fails.
	environ := inv.environ
	args, reexecErr := Harden(Process{
		Argv:        append([]string{program}, args...),
		Environ:     inv.environ,
		Credentials: inv.creds,
		Exec: func(path string, argv, env []string) error {
			inv.execs = append(inv.execs, execCall{path, argv, env})
			return errors.New("exec: permission denied")
		},
		ReplaceEnv: func(env []string) {
			inv.replacedEnv = env
			environ = env
		},
	})
	now := inv.now
	if now == nil {
		now = func() time.Time { return testNow }
	}
	var configFS fs.FS = fsys
	if inv.configFS != nil {
		configFS = inv.configFS
	}
	var stdout io.Writer = &inv.stdout
	if inv.stdoutWriter != nil {
		stdout = inv.stdoutWriter
	}
	fallback := newStderrLog(&inv.stderr, inv.creds.isElevated())
	deps := Deps{
		NewLogger: func(tag string) *slog.Logger {
			if inv.isSyslogDown {
				return newFallbackLogger(tag, func(string) (*syslog.Writer, error) { return nil, errors.New("dial unixgram /dev/log: no such file") }, fallback)
			}
			buf := &bytes.Buffer{}
			inv.logs[tag] = buf
			return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
		},
		ConfigFS:     configFS,
		ConfigPath:   SystemConfigPath,
		HTTP:         client,
		Hostname:     "host1.example.org",
		Now:          now,
		Program:      program,
		Stdout:       stdout,
		Stderr:       &inv.stderr,
		SetLogOutput: func(w io.Writer) { inv.logOutput = w },
		Credentials:  inv.creds,
		Environ:      environ,
		ReexecErr:    reexecErr,
		LookupUserName: func(uid int) (string, bool) {
			name, ok := testUsers[uid]
			return name, ok
		},
		IsLogOnStderr: func() bool { return inv.isSyslogDown },
		SpoolDir:      inv.spoolDir,
		CatchSignals:  inv.catchSignals,
		deliver:       inv.deliver,
		spoolSaved:    inv.spoolSaved,
		entryLocked:   inv.entryLocked,
	}
	ctx := inv.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	return Run(ctx, deps, args, inv.stdin)
}

// testNow is the clock of the invocations.
var testNow = time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)

// testUsers is the user database of the invocations.
var testUsers = map[int]string{0: "root", 990: "mailcrier", 1000: "alice"}

// callField is the random call id on every record; log and output drop it
// so that tests can match the fields after it.
var callField = regexp.MustCompile(` call=[0-9a-f]{16}`)

// output returns everything the invocation wrote: all syslog tags and
// stderr.
func (inv *invocation) output() string {
	var all strings.Builder
	for tag, buf := range inv.logs {
		all.WriteString("[" + tag + "]\n" + buf.String())
	}
	return callField.ReplaceAllString(all.String(), "") + "[stderr]\n" + inv.stderr.String()
}

// queuedAhead finds the id of the own message in the record that writes it
// ahead into the queue.
var queuedAhead = regexp.MustCompile(`msg="message queued ahead of delivery"(?: msgid=\S+)? id=(\S+)`)

// ownID returns the spool id of the own message of the invocation, empty
// when it has none.
func (inv *invocation) ownID() string {
	if m := queuedAhead.FindStringSubmatch(inv.output()); m != nil {
		return m[1]
	}
	return ""
}

func (inv *invocation) log(tag string) string {
	if buf, ok := inv.logs[tag]; ok {
		return callField.ReplaceAllString(buf.String(), "")
	}
	return ""
}

func httpTargetConfig(url string) string {
	return "[target.hook]\ntype = \"http\"\npreset = \"generic-json\"\nurl = \"" + url + "\"\n"
}

// TestRunDeliversToHTTPTarget runs a whole invocation against a local
// receiver: the message read from stdin arrives as the generic-json
// document and the exit status is 0.
func TestRunDeliversToHTTPTarget(t *testing.T) {
	type request struct {
		method, path, contentType string
		body                      map[string]string
	}
	requests := make(chan request, 1)
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("receiver: body is not a JSON object: %v", err)
		}
		requests <- request{r.Method, r.URL.Path, r.Header.Get("Content-Type"), body}
	}))
	defer server.Close()

	inv := &invocation{config: httpTargetConfig(server.URL + "/hook"), stdin: strings.NewReader("Subject: t\n\nb\n")}
	if code := inv.run(t); code != 0 {
		t.Fatalf("Run() = %d, want 0; log:\n%s", code, inv.log("mailcrier"))
	}
	// Run has returned, so a delivered request is already in the channel.
	var got request
	select {
	case got = <-requests:
	default:
		t.Fatal("receiver got no request")
	}
	want := request{"POST", "/hook", "application/json", map[string]string{"subject": "t", "body": "b\n", "hostname": "host1.example.org"}}
	if got.method != want.method || got.path != want.path || got.contentType != want.contentType {
		t.Errorf("request = %s %s %s, want %s %s %s", got.method, got.path, got.contentType, want.method, want.path, want.contentType)
	}
	for key, value := range want.body {
		if got.body[key] != value {
			t.Errorf("body[%q] = %q, want %q", key, got.body[key], value)
		}
	}
	if len(got.body) != len(want.body) {
		t.Errorf("body = %v, want exactly %v", got.body, want.body)
	}
	if log := inv.log("mailcrier"); !strings.Contains(log, "target delivered") || !strings.Contains(log, "target=hook") {
		t.Errorf("log does not record the delivery:\n%s", log)
	}
}

func TestRunUsesConfiguredSyslogTag(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()

	inv := &invocation{
		config: "[general]\nsyslog_tag = \"mailbot\"\n\n" + httpTargetConfig(server.URL),
		stdin:  strings.NewReader("Subject: t\n\nb\n"),
	}
	if code := inv.run(t); code != 0 {
		t.Fatalf("Run() = %d, want 0", code)
	}
	if !strings.Contains(inv.log("mailbot"), "target delivered") {
		t.Errorf("delivery not logged under the configured tag; logs: %v", inv.logs)
	}
	// A filter on the tag sees the message itself too.
	if !strings.Contains(inv.log("mailbot"), `msg="message received"`) || strings.Contains(inv.log("mailcrier"), "message received") {
		t.Errorf("message received not under the configured tag alone:\n%s", inv.output())
	}
}

// TestLogBeforeUserLine pins that without syslog the records on stderr
// come before what the user reads there: the report of --check-config,
// whose last line counts the findings, and the line of a usage error.
func TestLogBeforeUserLine(t *testing.T) {
	t.Run("check-config", func(t *testing.T) {
		inv := &invocation{config: "[target.backup]\ntype = \"discord\"\nurl = \"https://example.org/x\"\n[[route]]\nsubject = \"*\"\ntargets = [\"backup\"]\n",
			args: []string{"--check-config"}, creds: plainUser, stdin: iotest.ErrReader(errors.New("stdin read")), isSyslogDown: true}
		if code := inv.run(t); code != 0 {
			t.Fatalf("Run() = %d, want 0; stderr:\n%s", code, inv.stderr.String())
		}
		lines := strings.Split(strings.TrimSuffix(inv.stderr.String(), "\n"), "\n")
		report := slices.IndexFunc(lines, func(line string) bool { return strings.HasPrefix(line, "warning: ") })
		if report < 0 || lines[len(lines)-1] != "/etc/mailcrier.conf: 0 errors, 1 warning" ||
			slices.ContainsFunc(lines[report:], func(line string) bool { return strings.Contains(line, "level=") }) ||
			!slices.ContainsFunc(lines[:report], func(line string) bool { return strings.Contains(line, `msg="configuration checked"`) }) {
			t.Errorf("stderr mixes the log into the report:\n%s", inv.stderr.String())
		}
	})
	t.Run("usage", func(t *testing.T) {
		inv := &invocation{args: []string{"-t", "-f"}, stdin: strings.NewReader(""), isSyslogDown: true}
		if code := inv.run(t); code != 64 {
			t.Fatalf("Run() = %d, want 64", code)
		}
		lines := strings.Split(strings.TrimSuffix(inv.stderr.String(), "\n"), "\n")
		if len(lines) != 3 || lines[2] != "mailcrier: option -f requires a value" || !strings.Contains(lines[1], `msg="command line rejected"`) {
			t.Errorf("stderr:\n%s", inv.stderr.String())
		}
	})
}

func TestRunRejectsConfiguration(t *testing.T) {
	cases := []struct {
		name    string
		config  string
		wantLog string
	}{
		{"unknown-key", httpTargetConfig("http://127.0.0.1:1") + "colour = \"red\"\n", `unknown key \"target.hook.colour\"`},
		{"unparsable", "[target.hook\n", "expected"},
		{"no-targets", "", "no targets configured"},
		{"ntfy-without-topic", "[target.nt]\ntype = \"ntfy\"\nurl = \"https://ntfy.example.org/\"\n", `target \"nt\": URL has no topic`},
		{"shoutrrr-unknown-service", "[target.bus]\ntype = \"shoutrrr\"\nurl = \"nosuch://example.org\"\n", shoutrrrRejection},
		{"preset-missing", "[target.mm]\ntype = \"http\"\nurl = \"http://127.0.0.1:1\"\n", `one of keys \"preset\", \"template\" and \"template_file\" is required`},
	}
	for _, tc := range cases {
		t.Run("T-ADJ-28/"+tc.name, func(t *testing.T) {
			inv := &invocation{config: tc.config, stdin: strings.NewReader("Subject: t\n\nb\n")}
			if code := inv.run(t); code != 78 {
				t.Fatalf("Run() = %d, want 78", code)
			}
			log := inv.log("mailcrier")
			if !strings.Contains(log, "level=ERROR") || !strings.Contains(log, "configuration rejected") || !strings.Contains(log, tc.wantLog) {
				t.Errorf("log lacks the error record with %q:\n%s", tc.wantLog, log)
			}
		})
	}
}

func TestRunReportsUndeliveredMessage(t *testing.T) {
	cases := []struct {
		name   string
		status int
	}{
		{"rejected-by-receiver", http.StatusBadRequest},
		{"receiver-error-without-spool", http.StatusBadGateway},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
			}))
			defer server.Close()

			inv := &invocation{config: httpTargetConfig(server.URL), stdin: strings.NewReader("Subject: t\n\nb\n")}
			if code := inv.run(t); code != 69 {
				t.Fatalf("Run() = %d, want 69", code)
			}
			if log := inv.log("mailcrier"); !strings.Contains(log, "target failed") {
				t.Errorf("log lacks the failure:\n%s", log)
			}
		})
	}
}

func TestRunReportsUnreadableInput(t *testing.T) {
	inv := &invocation{config: httpTargetConfig("http://127.0.0.1:1"), stdin: iotest.ErrReader(errors.New("input/output error"))}
	if code := inv.run(t); code != 66 {
		t.Fatalf("Run() = %d, want 66", code)
	}
}

// hangingServer accepts connections and never answers until the test ends.
func hangingServer(t *testing.T) *httptest.Server {
	t.Helper()
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	t.Cleanup(func() {
		close(release)
		server.Close()
	})
	return server
}

// TestRunTimeLimits pins that a receiver which never answers cannot hold
// the process: the request timeout ends one request, the call deadline ends
// the whole delivery. Each limit is set far below the other, so the elapsed
// time shows which one fired.
func TestRunTimeLimits(t *testing.T) {
	cases := []struct {
		name    string
		general string
		targets int
	}{
		{"T-ADJ-51/request-timeout", "http_timeout = \"50ms\"\ndeadline = \"1h\"\n", 1},
		{"T-ADJ-51/call-deadline", "http_timeout = \"1h\"\ndeadline = \"50ms\"\n", 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := hangingServer(t)
			config := "[general]\n" + tc.general
			for i := range tc.targets {
				config += fmt.Sprintf("\n[target.hook%d]\ntype = \"http\"\npreset = \"generic-json\"\nurl = \"%s\"\n", i, server.URL)
			}
			inv := &invocation{config: config, stdin: strings.NewReader("Subject: t\n\nb\n")}
			start := time.Now()
			code := inv.run(t)
			if elapsed := time.Since(start); elapsed > 10*time.Second {
				t.Errorf("Run() took %v, want the 50ms limit to end it", elapsed)
			}
			if code != 69 {
				t.Errorf("Run() = %d, want 69", code)
			}
			if log := inv.log("mailcrier"); strings.Count(log, "target failed") != tc.targets {
				t.Errorf("log does not record %d failed targets:\n%s", tc.targets, log)
			}
		})
	}
}

// TestRunKeepsMessageOutOfLogs pins that the log carries the size and the
// Message-ID of the message but neither its headers nor its body, on
// success, on a rejection whose response echoes the request, and on a
// configuration error.
func TestRunKeepsMessageOutOfLogs(t *testing.T) {
	const marker = "MARKER-7f3a"
	input := "Subject: disk " + marker + "\nMessage-ID: <42@db1.example.org>\nX-Job: " + marker + "\n\nbody " + marker + "\n"
	echo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.Copy(w, r.Body)
	}))
	defer echo.Close()
	accept := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer accept.Close()

	cases := []struct {
		name     string
		config   string
		wantCode int
	}{
		{"delivered", httpTargetConfig(accept.URL), 0},
		{"rejected-with-echo", httpTargetConfig(echo.URL), 69},
		{"configuration-error", "", 78},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inv := &invocation{config: tc.config, stdin: strings.NewReader(input)}
			if code := inv.run(t); code != tc.wantCode {
				t.Errorf("Run() = %d, want %d", code, tc.wantCode)
			}
			output := inv.output()
			if strings.Contains(output, marker) {
				t.Errorf("output quotes the message:\n%s", output)
			}
			want := fmt.Sprintf(`msg="message received" msgid=<42@db1.example.org> size=%d`, len(input))
			if !strings.Contains(output, want) {
				t.Errorf("output lacks %q:\n%s", want, output)
			}
		})
	}
}

// TestRunLogsResultFields pins the logfmt fields of a failed target, which
// operators filter syslog by.
func TestRunLogsResultFields(t *testing.T) {
	server := echoServer(t, http.StatusBadGateway)
	inv := &invocation{config: httpTargetConfig(server.URL), stdin: strings.NewReader("Message-ID: <1@h>\n\nb\n")}
	if code := inv.run(t); code != 69 {
		t.Fatalf("Run() = %d, want 69", code)
	}
	want := `level=ERROR msg="target failed" msgid=<1@h> target=hook class=temp status=502 err=`
	if log := inv.log("mailcrier"); !strings.Contains(log, want) {
		t.Errorf("log lacks %q:\n%s", want, log)
	}
}

// TestRunTagsRecordsWithCall pins the fields that tie records together:
// every record of a call carries the same call id, and msgid appears only
// when the message has a Message-ID, cut to 256 bytes.
func TestRunTagsRecordsWithCall(t *testing.T) {
	server := echoServer(t, http.StatusBadGateway)
	cases := []struct {
		name      string
		header    string
		wantMsgID string
	}{
		{"no-message-id", "", ""},
		{"message-id", "Message-ID: <1@h>\n", "msgid=<1@h> "},
		{"long-message-id", "Message-ID: <" + strings.Repeat("x", 1000) + ">\n", "msgid=<" + strings.Repeat("x", 255) + " "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inv := &invocation{config: httpTargetConfig(server.URL), stdin: strings.NewReader(tc.header + "Subject: t\n\nb\n")}
			if code := inv.run(t); code != 69 {
				t.Fatalf("Run() = %d, want 69", code)
			}
			raw := inv.logs["mailcrier"].String()
			lines := strings.Split(strings.TrimSpace(raw), "\n")
			ids := map[string]bool{}
			for _, line := range lines {
				ids[callField.FindString(line)] = true
			}
			if len(lines) < 2 || len(ids) != 1 || ids[""] {
				t.Errorf("records do not share one call id:\n%s", raw)
			}
			if got := inv.log("mailcrier"); tc.wantMsgID == "" && strings.Contains(got, "msgid=") ||
				tc.wantMsgID != "" && !strings.Contains(got, `msg="target failed" `+tc.wantMsgID+"target=hook") {
				t.Errorf("msgid field wrong, want %q:\n%s", tc.wantMsgID, got)
			}
		})
	}
}

// TestRunLogsSizeLimit pins the fields of the warning for a message over
// the size limit: the bytes read and the limit, which say how much was
// lost.
func TestRunLogsSizeLimit(t *testing.T) {
	input := "Subject: big\n\n" + strings.Repeat("x", message.MaxSize)
	inv := &invocation{config: twoTargets, stdin: strings.NewReader(input), deliver: (&recorder{}).deliver}
	if code := inv.run(t); code != 0 {
		t.Fatalf("Run() = %d, want 0", code)
	}
	want := fmt.Sprintf(`level=WARN msg="message over the size limit, rest discarded" size=%d max_size=%d`, len(input), message.MaxSize)
	if !strings.Contains(inv.log("mailcrier"), want) {
		t.Errorf("log lacks %q:\n%.2000s", want, inv.log("mailcrier"))
	}
}
