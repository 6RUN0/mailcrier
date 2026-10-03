package hook

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/6RUN0/mailcrier/internal/backend"
)

// writeHook writes an executable shell script with body into a new
// directory and returns its path and the directory.
func writeHook(t *testing.T, body string) (path, dir string) {
	t.Helper()
	dir = t.TempDir()
	path = filepath.Join(dir, "hook")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path, dir
}

// recordingHook writes its arguments, environment and stdin into files of
// its directory, one argument per line.
const recordingHook = `printf '%s\n' "$@" > "$0.argv"
env > "$0.env"
cat > "$0.stdin"
`

// readEnv returns the environment a recording hook saw.
func readEnv(t *testing.T, path string) map[string]string {
	t.Helper()
	content, err := os.ReadFile(path + ".env")
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{}
	for line := range strings.Lines(string(content)) {
		key, value, _ := strings.Cut(strings.TrimSuffix(line, "\n"), "=")
		env[key] = value
	}
	return env
}

func testMessage() *backend.Message {
	return &backend.Message{
		Raw:     []byte("From: cron@example.org\nTo: ops@example.org\nSubject: backup\n\nok\n"),
		Subject: "backup", From: "cron@example.org", To: []string{"ops@example.org", "dev@example.org"},
		MessageID: "<1@example.org>", Hostname: "db1.example.org",
	}
}

func classOf(t *testing.T, err error) backend.Class {
	t.Helper()
	var deliveryErr *backend.Error
	if !errors.As(err, &deliveryErr) {
		t.Fatalf("Send() error = %v, want *backend.Error", err)
	}
	return deliveryErr.Class
}

func TestSendPassesMessageAndEnvironment(t *testing.T) {
	path, _ := writeHook(t, recordingHook)
	msg := testMessage()
	sender := New(Options{Name: "run", Argv: []string{path, "--flag", "two words"}, Process: Process{TZ: "Europe/Berlin"}})
	if err := sender.Send(context.Background(), backend.Payload{Text: "ignored", Message: msg}); err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	if argv, _ := os.ReadFile(path + ".argv"); string(argv) != "--flag\ntwo words\n" {
		t.Errorf("argv = %q", argv)
	}
	if stdin, _ := os.ReadFile(path + ".stdin"); !bytes.Equal(stdin, msg.Raw) {
		t.Errorf("stdin = %q, want the message", stdin)
	}
	env := readEnv(t, path)
	want := map[string]string{
		"PATH": "/usr/sbin:/usr/bin:/sbin:/bin", "LANG": "C.UTF-8", "TZ": "Europe/Berlin",
		"MAILCRIER_SUBJECT": "backup", "MAILCRIER_FROM": "cron@example.org", "MAILCRIER_TO": "ops@example.org, dev@example.org",
		"MAILCRIER_HOSTNAME": "db1.example.org", "MAILCRIER_TARGET": "run", "MAILCRIER_MSGID": "<1@example.org>", "MAILCRIER_SIZE": "63",
	}
	for key, value := range want {
		if env[key] != value {
			t.Errorf("%s = %q, want %q", key, env[key], value)
		}
	}
	// The shell adds PWD and the like; nothing of the test process passes.
	if _, ok := env["HOME"]; ok {
		t.Errorf("HOME passed to the hook: %v", env)
	}
}

// TestSendDoesNotInterpretSubject pins that shell syntax and line breaks
// in the subject stay data: nothing runs, argv is unchanged, and the
// variable keeps the text on one line.
func TestSendDoesNotInterpretSubject(t *testing.T) {
	path, dir := writeHook(t, recordingHook)
	msg := testMessage()
	msg.Subject = "$(touch " + dir + "/a); touch " + dir + "/b `touch " + dir + "/c`\nX-Injected: 1\r\n|| touch " + dir + "/d"
	if err := New(Options{Name: "run", Argv: []string{path, "$MAILCRIER_SUBJECT"}}).Send(context.Background(), backend.Payload{Message: msg}); err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	for _, name := range []string{"a", "b", "c", "d"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			t.Errorf("file %s created: the subject was executed", name)
		}
	}
	if argv, _ := os.ReadFile(path + ".argv"); string(argv) != "$MAILCRIER_SUBJECT\n" {
		t.Errorf("argv = %q, want the configured argument only", argv)
	}
	wantSubject := strings.NewReplacer("\r", " ", "\n", " ").Replace(msg.Subject)
	if got := readEnv(t, path)["MAILCRIER_SUBJECT"]; got != wantSubject {
		t.Errorf("MAILCRIER_SUBJECT = %q, want %q", got, wantSubject)
	}
}

// TestSendCutsLongValues pins that a subject of 200 KB, over the limit of
// one execve string, still starts the hook: the variable is cut to 4 KiB
// at a character, the message goes whole on stdin.
func TestSendCutsLongValues(t *testing.T) {
	path, _ := writeHook(t, recordingHook)
	msg := testMessage()
	msg.Subject = "x" + strings.Repeat("я", 100<<10)
	msg.Raw = []byte("Subject: " + msg.Subject + "\n\nbody\n")
	if err := New(Options{Name: "run", Argv: []string{path}}).Send(context.Background(), backend.Payload{Message: msg}); err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	subject := readEnv(t, path)["MAILCRIER_SUBJECT"]
	if want := "x" + strings.Repeat("я", (maxEnvValue-1)/2); subject != want {
		t.Errorf("MAILCRIER_SUBJECT has %d bytes, want %d cut at a character", len(subject), len(want))
	}
	if stdin, _ := os.ReadFile(path + ".stdin"); !bytes.Equal(stdin, msg.Raw) {
		t.Errorf("stdin has %d bytes, want the message of %d", len(stdin), len(msg.Raw))
	}
}

// TestBuildEnvBoundsTotal pins the bound of the whole environment, which
// seven values of 4 KiB do not reach.
func TestBuildEnvBoundsTotal(t *testing.T) {
	long := strings.Repeat("v", 2*maxEnvValue)
	msg := &backend.Message{Subject: long, From: long, To: slices.Repeat([]string{long}, 20), MessageID: long, Hostname: long}
	env := buildEnv(Options{Name: "n"}, msg)
	total := 0
	for _, entry := range env {
		total += len(entry) + 1
		if key, value, _ := strings.Cut(entry, "="); len(value) > maxEnvValue {
			t.Errorf("%s has %d bytes", key, len(value))
		}
	}
	if total > maxEnvTotal {
		t.Errorf("environment has %d bytes, want at most %d", total, maxEnvTotal)
	}
	if len(env) != len(fixedEnv)+7 {
		t.Errorf("environment has %d entries, want all variables", len(env))
	}
}

func TestSendExitStatus(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       backend.Class
	}{
		{"tempfail", "exit 75\n", backend.Temporary},
		{"failure", "exit 1\n", backend.Permanent},
		{"signal", "kill -TERM $$\n", backend.Permanent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, _ := writeHook(t, tc.body)
			err := New(Options{Name: "run", Argv: []string{path}}).Send(context.Background(), backend.Payload{Message: testMessage()})
			if got := classOf(t, err); got != tc.want {
				t.Errorf("class = %v, want %v (%v)", got, tc.want, err)
			}
		})
	}
}

// TestSendExitStatusWinsOverTimeout pins that a hook which exited 1 on
// its own fails for good, though the timeout ran out while a child it
// left behind held its stdout.
func TestSendExitStatusWinsOverTimeout(t *testing.T) {
	// The child outlives the wait for the pipes by little, so that it does
	// not linger after the test.
	path, _ := writeHook(t, "sleep 3 &\nexit 1\n")
	err := New(Options{Name: "run", Argv: []string{path}, Timeout: 100 * time.Millisecond}).Send(context.Background(), backend.Payload{Message: testMessage()})
	if got := classOf(t, err); got != backend.Permanent {
		t.Errorf("class = %v, want permanent (%v)", got, err)
	}
}

func TestSendNotStarted(t *testing.T) {
	dir := t.TempDir()
	notExecutable := filepath.Join(dir, "plain")
	if err := os.WriteFile(notExecutable, []byte("#!/bin/sh\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, argv := range map[string][]string{
		"missing":        {filepath.Join(dir, "missing")},
		"not-executable": {notExecutable},
		"argument-nul":   {"/bin/sh", "a\x00b"},
	} {
		t.Run(name, func(t *testing.T) {
			err := New(Options{Name: "run", Argv: argv}).Send(context.Background(), backend.Payload{Message: testMessage()})
			if got := classOf(t, err); got != backend.Permanent {
				t.Errorf("class = %v, want permanent (%v)", got, err)
			}
		})
	}
}

// TestSendTimeout pins that a hook past the timeout of the target, or past
// the deadline of the context when that comes first, is a temporary
// failure.
func TestSendTimeout(t *testing.T) {
	path, _ := writeHook(t, "exec sleep 1000\n")
	for name, tc := range map[string]struct {
		timeout, deadline time.Duration
	}{
		"target-timeout":   {timeout: 50 * time.Millisecond, deadline: time.Hour},
		"context-deadline": {timeout: time.Hour, deadline: 50 * time.Millisecond},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), tc.deadline)
			defer cancel()
			err := New(Options{Name: "run", Argv: []string{path}, Timeout: tc.timeout}).Send(ctx, backend.Payload{Message: testMessage()})
			if got := classOf(t, err); got != backend.Temporary {
				t.Errorf("class = %v, want temporary (%v)", got, err)
			}
		})
	}
}

// TestSendKillsProcessGroup pins that the end of a run kills the children
// of the hook too. A child holds the write end of a FIFO; the reader sees
// EOF only once no process holds it, so EOF proves the child dead without
// waiting for a time. The run ends by cancelling its context, the path a
// timeout takes as well.
func TestSendKillsProcessGroup(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	path, _ := writeHook(t, "(printf started; exec sleep 1000) > "+fifo+" &\nwait\n")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	childEOF := make(chan error, 1)
	go func() {
		// Opening blocks until the child opens the write end.
		reader, err := os.Open(fifo)
		if err != nil {
			childEOF <- err
			cancel()
			return
		}
		defer func() { _ = reader.Close() }()
		started := make([]byte, len("started"))
		if _, err := io.ReadFull(reader, started); err != nil {
			childEOF <- err
			cancel()
			return
		}
		cancel()
		if err := reader.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
			childEOF <- err
			return
		}
		_, err = reader.Read(make([]byte, 1))
		childEOF <- err
	}()

	err := New(Options{Name: "run", Argv: []string{path}}).Send(ctx, backend.Payload{Message: testMessage()})
	if got := classOf(t, err); got != backend.Temporary {
		// A hook that never opened the FIFO would leave the reader blocked.
		t.Fatalf("class = %v, want temporary (%v)", got, err)
	}
	if err := <-childEOF; !errors.Is(err, io.EOF) {
		t.Errorf("FIFO read = %v, want EOF: the child of the hook survived", err)
	}
}

// TestSendDropsGroupWithCredential pins that a credential of the real ids
// starts the hook without privilege: the setgid case itself runs in
// TestSetgidHookDropsGroup.
func TestSendDropsGroupWithCredential(t *testing.T) {
	path, _ := writeHook(t, "exit 0\n")
	credential := &syscall.Credential{Uid: uint32(os.Getuid()), Gid: uint32(os.Getgid()), NoSetGroups: true}
	if err := New(Options{Name: "run", Argv: []string{path}, Process: Process{Credential: credential}}).Send(context.Background(), backend.Payload{Message: testMessage()}); err != nil {
		t.Fatalf("Send() error = %v", err)
	}
}

// TestSendLogsOutput pins one record per run with stdout and stderr cut to
// 4 KiB and their full size, at warning level for a failed run.
func TestSendLogsOutput(t *testing.T) {
	path, _ := writeHook(t, "echo out; echo err >&2; head -c 10000 /dev/zero | tr '\\0' x; exit 1\n")
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, nil))
	err := New(Options{Name: "run", Argv: []string{path}, Process: Process{Log: log}}).Send(context.Background(), backend.Payload{Message: testMessage()})
	if err == nil {
		t.Fatal("Send() succeeded on exit status 1")
	}
	records := strings.Split(strings.TrimSpace(logs.String()), "\n")
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1:\n%s", len(records), logs.String())
	}
	record := records[0]
	for _, want := range []string{`"level":"WARN"`, `"msg":"hook output"`, `"target":"run"`, `"output":"out\nerr\nxxx`, `"output_size":10008`} {
		if !strings.Contains(record, want) {
			t.Errorf("record lacks %s:\n%s", want, record)
		}
	}
	if strings.Count(record, "x") > maxOutput {
		t.Errorf("record keeps more than %d bytes of output", maxOutput)
	}
}

func TestSendWithoutMessage(t *testing.T) {
	err := New(Options{Name: "run", Argv: []string{"/bin/true"}}).Send(context.Background(), backend.Payload{Text: "t"})
	if got := classOf(t, err); got != backend.Permanent {
		t.Errorf("class = %v, want permanent", got)
	}
}
