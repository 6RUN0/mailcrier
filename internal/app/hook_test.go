package app

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/6RUN0/mailcrier/internal/spool"
)

// writeHookScript writes an executable shell script into a new directory
// and returns its path.
func writeHookScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hook")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestRunDeliversToExecTarget runs a whole invocation with a hook that
// records what it gets: the message without Bcc on stdin, a subject of
// 200 KB cut in the environment, no blind copy anywhere, and its output
// in the log with the secrets of the configuration masked.
func TestRunDeliversToExecTarget(t *testing.T) {
	const secretURL = "http://127.0.0.1:1/api/webhooks/SECRET-TOKEN-0123456789"
	path := writeHookScript(t, "env > \"$0.env\"\ncat > \"$0.stdin\"\necho \"posting to "+secretURL+"\"\n")
	config := "[target.run]\ntype = \"exec\"\nargv = [\"" + path + "\"]\n\n" +
		"[target.other]\ntype = \"discord\"\nurl = \"" + secretURL + "\"\n"
	subject := strings.Repeat("s", 200<<10)
	input := "Subject: " + subject + "\nTo: ops@example.org\nBcc: hidden@example.org\n\nbody\n"
	inv := &invocation{config: config, args: []string{"-t"}, stdin: strings.NewReader(input), environ: []string{"TZ=Europe/Berlin"}}
	inv.run(t)

	stdin, err := os.ReadFile(path + ".stdin")
	if err != nil {
		t.Fatalf("hook did not run: %v; log:\n%s", err, inv.output())
	}
	if want := "Subject: " + subject + "\nTo: ops@example.org\n\nbody\n"; string(stdin) != want {
		t.Errorf("stdin has %d bytes, want the message of %d without Bcc", len(stdin), len(want))
	}
	envFile, err := os.ReadFile(path + ".env")
	if err != nil {
		t.Fatal(err)
	}
	env := string(envFile)
	for _, want := range []string{"\nMAILCRIER_SUBJECT=" + strings.Repeat("s", 4096) + "\n", "\nMAILCRIER_TO=ops@example.org\n", "\nMAILCRIER_TARGET=run\n", "\nTZ=Europe/Berlin\n"} {
		if !strings.Contains("\n"+env, want) {
			t.Errorf("environment lacks %.80q:\n%.300s", want, env)
		}
	}
	if strings.Contains(env, "hidden") || strings.Contains(string(stdin), "hidden") {
		t.Error("the blind copy reached the hook")
	}
	log := inv.log("mailcrier")
	if !strings.Contains(log, `level=INFO msg="hook output" target=run output="posting to ***\n"`) || strings.Contains(log, "SECRET-TOKEN") {
		t.Errorf("hook output not logged masked:\n%s", log)
	}
}

// TestRunQueuesHookTempFailure pins that exit status 75 of a hook keeps
// the message in the queue and exits 0, as a temporary failure of any
// target does, and that the output of the hook names the spool entry, in
// the call and in a queue run.
func TestRunQueuesHookTempFailure(t *testing.T) {
	path := writeHookScript(t, "cat >/dev/null\necho busy\nexit 75\n")
	dir := t.TempDir()
	config := "[target.run]\ntype = \"exec\"\nargv = [\"" + path + "\"]\n"
	clock := newClock()
	inv := &invocation{config: config, stdin: strings.NewReader("Subject: t\n\nb\n"), spoolDir: dir, now: clock.now}
	if code := inv.run(t); code != 0 {
		t.Fatalf("Run() = %d, want 0; log:\n%s", code, inv.output())
	}
	entries, err := filepath.Glob(filepath.Join(dir, spool.QueueDir, "*.eml"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("queue holds %v, want one entry", entries)
	}
	id := strings.TrimSuffix(filepath.Base(entries[0]), ".eml")
	want := `level=WARN msg="hook output" id=` + id + ` target=run output="busy\n"`
	if !strings.Contains(inv.output(), want) {
		t.Errorf("output lacks %q:\n%s", want, inv.output())
	}
	clock.advance(time.Hour)
	queueRun := &invocation{config: config, args: []string{"-q"}, creds: rootCaller, spoolDir: dir, now: clock.now}
	if code := queueRun.run(t); code != 0 || !strings.Contains(queueRun.output(), want) {
		t.Errorf("-q = %d, want 0 and %q; output:\n%s", code, want, queueRun.output())
	}
}

// TestHookProcess pins the ids a hook runs with: the real ones when the
// process has the group of the setgid binary, those of the process
// otherwise.
func TestHookProcess(t *testing.T) {
	for _, tc := range []struct {
		name  string
		creds Credentials
		want  *syscall.Credential
	}{
		{"elevated", elevatedUser, &syscall.Credential{Uid: 1000, Gid: 1000, NoSetGroups: true}},
		{"root-setgid", Credentials{UID: 0, GID: 0, EGID: 990}, &syscall.Credential{Uid: 0, Gid: 0, NoSetGroups: true}},
		{"service-user", Credentials{UID: 990, GID: 990, EGID: 990, ServiceUID: 990}, nil},
		{"plain", Credentials{UID: 1000, GID: 1000, EGID: 1000}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := hookProcess(Deps{Credentials: tc.creds, Environ: []string{"TZ=UTC"}}, nil)
			if !reflect.DeepEqual(got.Credential, tc.want) {
				t.Errorf("Credential = %+v, want %+v", got.Credential, tc.want)
			}
			if got.TZ != "UTC" {
				t.Errorf("TZ = %q", got.TZ)
			}
		})
	}
}
