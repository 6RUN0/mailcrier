package app

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/6RUN0/slendmail/internal/spool"
)

// openStartedFIFO creates a FIFO in a new directory and returns its path
// and a channel that yields once a process opened its write end and wrote
// "started", then the error of the read that follows: io.EOF once no
// process holds the write end any more.
func openStartedFIFO(t *testing.T) (string, <-chan error) {
	t.Helper()
	fifo := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	events := make(chan error, 2)
	go func() {
		// Opening blocks until the hook opens the write end.
		reader, err := os.Open(fifo)
		if err != nil {
			events <- err
			return
		}
		defer func() { _ = reader.Close() }()
		started := make([]byte, len("started"))
		if _, err := io.ReadFull(reader, started); err != nil {
			events <- err
			return
		}
		events <- nil
		if err := reader.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
			events <- err
			return
		}
		_, err = reader.Read(make([]byte, 1))
		events <- err
	}()
	return fifo, events
}

// hookHelperArgs returns the arguments of a helper that delivers one
// message to an exec target running script, with a spool in dir.
func hookHelperArgs(t *testing.T, dir, script string) helperArgs {
	t.Helper()
	path := writeHookScript(t, script)
	return helperArgs{
		Config:   "[target.run]\ntype = \"exec\"\nargv = [\"" + path + "\"]\ntimeout = \"1h\"\n",
		SpoolDir: dir,
		Creds:    Credentials{UID: os.Getuid(), GID: os.Getgid(), EGID: os.Getegid(), ServiceUID: -1},
		Args:     []string{"-ti"},
		Stdin:    "Subject: hook\n\nbody\n",
	}
}

// TestSpoolHookSignal pins that SIGTERM to slendmail during a hook kills
// the process group of the hook, a child of the hook included, and keeps
// the message queued with a temporary failure; the call exits 0, as for
// any queued message. The FIFO held by the child reaches EOF only once it
// is dead, so nothing waits for a time.
func TestSpoolHookSignal(t *testing.T) {
	fifo, events := openStartedFIFO(t)
	dir := t.TempDir()
	h := startHelper(t, helperSignals, hookHelperArgs(t, dir, "(printf started; exec sleep 1000) > "+fifo+" &\nwait\n"))
	if err := <-events; err != nil {
		t.Fatalf("hook did not start: %v; output:\n%s", err, h.output.String())
	}
	if err := h.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := <-events; !errors.Is(err, io.EOF) {
		t.Errorf("FIFO read = %v, want EOF: the child of the hook survived", err)
	}
	if err := h.cmd.Wait(); err != nil {
		t.Fatalf("slendmail after SIGTERM = %v, want exit 0; output:\n%s", err, h.output.String())
	}
	run := readTargetState(t, dir, "run")
	if run.LastClass != "temp" || !strings.Contains(run.LastError, "hook killed") {
		t.Errorf("target state %+v, want a temporary failure of the killed hook", run)
	}
}

// TestSpoolHookParentKilled pins that SIGKILL of slendmail, which no
// handler sees, still kills the hook itself through Pdeathsig.
func TestSpoolHookParentKilled(t *testing.T) {
	fifo, events := openStartedFIFO(t)
	h := startHelper(t, helperSignals, hookHelperArgs(t, t.TempDir(), "exec 3> "+fifo+"\nprintf started >&3\nexec sleep 1000\n"))
	if err := <-events; err != nil {
		t.Fatalf("hook did not start: %v; output:\n%s", err, h.output.String())
	}
	if err := h.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := <-events; !errors.Is(err, io.EOF) {
		t.Errorf("FIFO read = %v, want EOF: the hook survived slendmail", err)
	}
	_ = h.cmd.Wait()
}

// TestSpoolSignalWhileTyping pins that SIGINT before the end of the
// message, a Ctrl-C while it is typed, ends slendmail with the default
// action and leaves nothing in the spool: signals are caught only once the
// message is read.
func TestSpoolSignalWhileTyping(t *testing.T) {
	dir := t.TempDir()
	args := hookHelperArgs(t, dir, "exit 0\n")
	args.ReadStdin = true
	h := startHelper(t, helperSignals, args)
	if line := h.readReport(t); line != "ready" {
		t.Fatalf("helper reported %q", line)
	}
	if _, err := h.stdin.WriteString("Subject: draft\n\nhalf written"); err != nil {
		t.Fatal(err)
	}
	if err := h.cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	// Closed only now, so that a helper that survived the signal ends its
	// input and exits instead of waiting.
	_ = h.stdin.Close()
	_ = h.cmd.Wait()
	status, ok := h.cmd.ProcessState.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGINT {
		t.Errorf("helper ended with %v, want death by SIGINT; output:\n%s", h.cmd.ProcessState, h.output.String())
	}
	for _, area := range []string{spool.QueueDir, spool.TmpDir} {
		if entries, _ := filepath.Glob(filepath.Join(dir, area, "*")); len(entries) != 0 {
			t.Errorf("%s holds %v, want nothing", area, entries)
		}
	}
}

// TestSpoolHookIgnoredHangup pins that SIGHUP, ignored when slendmail
// started as under nohup, stays ignored. SIGHUP goes first and SIGTERM
// after it; the runtime hands pending signals over in ascending order, so
// a caught SIGHUP would cancel the call first and name itself as the
// cause. The cause recorded is that of SIGTERM.
func TestSpoolHookIgnoredHangup(t *testing.T) {
	fifo, events := openStartedFIFO(t)
	dir := t.TempDir()
	args := hookHelperArgs(t, dir, "(printf started; exec sleep 1000) > "+fifo+" &\nwait\n")
	args.IgnoreHangup = true
	h := startHelper(t, helperSignals, args)
	if err := <-events; err != nil {
		t.Fatalf("hook did not start: %v; output:\n%s", err, h.output.String())
	}
	for _, sig := range []syscall.Signal{syscall.SIGHUP, syscall.SIGTERM} {
		if err := h.cmd.Process.Signal(sig); err != nil {
			t.Fatal(err)
		}
	}
	if err := <-events; !errors.Is(err, io.EOF) {
		t.Errorf("FIFO read = %v, want EOF after SIGTERM", err)
	}
	if err := h.cmd.Wait(); err != nil {
		t.Fatalf("slendmail = %v, want exit 0; output:\n%s", err, h.output.String())
	}
	run := readTargetState(t, dir, "run")
	if !strings.Contains(run.LastError, "terminated") || strings.Contains(run.LastError, "hangup") {
		t.Errorf("last error %q, want the call cancelled by SIGTERM, not by the ignored SIGHUP", run.LastError)
	}
}

// targetState is the part of a sidecar target state the tests read.
type targetState struct {
	LastClass string `json:"last_class"`
	LastError string `json:"last_error"`
}

// readTargetState returns the state of target in the one entry of queue/
// in dir.
func readTargetState(t *testing.T, dir, target string) targetState {
	t.Helper()
	sidecars, err := filepath.Glob(filepath.Join(dir, spool.QueueDir, "*.json"))
	if err != nil || len(sidecars) != 1 {
		t.Fatalf("queue holds %v, want one entry", sidecars)
	}
	sidecar, err := os.ReadFile(sidecars[0])
	if err != nil {
		t.Fatal(err)
	}
	var entry struct {
		Targets map[string]targetState `json:"targets"`
	}
	if err := json.Unmarshal(sidecar, &entry); err != nil {
		t.Fatal(err)
	}
	return entry.Targets[target]
}
