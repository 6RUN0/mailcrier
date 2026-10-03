package app

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/fstest"
	"time"

	"github.com/6RUN0/slendmail/internal/message"
	"github.com/6RUN0/slendmail/internal/spool"
)

// Environment of the spool helper process: its mode, and the parameters
// of its one Run as JSON.
const (
	spoolHelperMode = "SLENDMAIL_SPOOL_HELPER"
	spoolHelperArgs = "SLENDMAIL_SPOOL_HELPER_ARGS"
)

// Modes of the spool helper process.
const (
	// helperKillAfterDone kills the process with SIGKILL right after the
	// first sidecar write that records a delivered target.
	helperKillAfterDone = "kill-after-done"
	// helperPauseAfterDone reports the first sidecar write that records a
	// delivered target on fd 3 and waits, holding the entry, until fd 4
	// reaches its end.
	helperPauseAfterDone = "pause-after-done"
	// helperStopWhenLocked reports the first entry a queue run locks on
	// fd 3 and stops the process with SIGSTOP; continued, it waits for the
	// end of fd 4 without doing anything else.
	helperStopWhenLocked = "stop-when-locked"
	// helperPlain runs without a hook.
	helperPlain = "plain"
	// helperSignals catches signals with CancelOnSignal, as main does.
	helperSignals = "signals"
)

// helperArgs are the parameters of the one Run of a helper process.
type helperArgs struct {
	Config   string
	SpoolDir string
	Creds    Credentials
	Args     []string
	Stdin    string
	// ReadStdin reads the message from the stdin of the process instead
	// of Stdin and reports "ready" on fd 3 at the first read, from inside
	// message.Read, then "catching" when Run starts to catch signals;
	// IgnoreHangup starts Run with SIGHUP ignored, as nohup
	// does.
	ReadStdin    bool
	IgnoreHangup bool
}

// TestSpoolHelper is not a test: it is the child process of the tests
// below, started as the test binary with spoolHelperMode set.
func TestSpoolHelper(t *testing.T) {
	mode := os.Getenv(spoolHelperMode)
	if mode == "" {
		return
	}
	var args helperArgs
	if err := json.Unmarshal([]byte(os.Getenv(spoolHelperArgs)), &args); err != nil {
		t.Fatal(err)
	}
	deps := Deps{
		NewLogger:      func(string) *slog.Logger { return slog.New(slog.NewTextHandler(os.Stderr, nil)) },
		ConfigFS:       fstest.MapFS{SystemConfigPath: {Data: []byte(args.Config)}},
		ConfigPath:     SystemConfigPath,
		HTTP:           &http.Client{},
		Hostname:       "host1.example.org",
		Now:            time.Now,
		Program:        "/usr/sbin/sendmail",
		Stdout:         os.Stdout,
		Stderr:         os.Stderr,
		SetLogOutput:   func(io.Writer) {},
		Credentials:    args.Creds,
		LookupUserName: func(int) (string, bool) { return "", false },
		SpoolDir:       args.SpoolDir,
	}
	report := os.NewFile(3, "report")
	var once sync.Once
	hasDone := func(e *spool.Entry) bool {
		for _, state := range e.Targets {
			if state.State == spool.Done {
				return true
			}
		}
		return false
	}
	switch mode {
	case helperKillAfterDone:
		deps.spoolSaved = func(e *spool.Entry) {
			if hasDone(e) {
				_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
			}
		}
	case helperPauseAfterDone:
		deps.spoolSaved = func(e *spool.Entry) {
			if !hasDone(e) {
				return
			}
			once.Do(func() {
				_, _ = fmt.Fprintln(report, e.ID)
				_, _ = io.Copy(io.Discard, os.NewFile(4, "resume"))
			})
		}
	case helperStopWhenLocked:
		deps.entryLocked = func(id string) {
			once.Do(func() {
				_, _ = fmt.Fprintln(report, id)
				_ = syscall.Kill(os.Getpid(), syscall.SIGSTOP)
				// A SIGCONT must not let the run go on: the test counts
				// on the entry staying locked and undelivered.
				_, _ = io.Copy(io.Discard, os.NewFile(4, "resume"))
			})
		}
	}
	var stdin io.Reader = strings.NewReader(args.Stdin)
	if mode == helperSignals {
		deps.CatchSignals = CancelOnSignal
		if args.IgnoreHangup {
			signal.Ignore(syscall.SIGHUP)
		}
		if args.ReadStdin {
			stdin = &readyReader{r: os.Stdin, report: report}
			deps.CatchSignals = func(parent context.Context) (context.Context, context.CancelFunc) {
				_, _ = fmt.Fprintln(report, "catching")
				return CancelOnSignal(parent)
			}
		}
	}
	os.Exit(Run(context.Background(), deps, args.Args, stdin))
}

// readyReader reports "ready" on report when Run first reads from r: a
// signal sent after the report reaches the process while it reads the
// message, past every point Run could catch signals before reading.
type readyReader struct {
	r      io.Reader
	report io.Writer
	once   sync.Once
}

func (rr *readyReader) Read(p []byte) (int, error) {
	rr.once.Do(func() { _, _ = fmt.Fprintln(rr.report, "ready") })
	return rr.r.Read(p)
}

// helper is a running helper process.
type helper struct {
	cmd *exec.Cmd
	// report yields the lines the helper writes on fd 3; resume, closed,
	// lets a paused helper go on.
	report *bufio.Reader
	resume *os.File
	output *strings.Builder
	// stdin writes to the stdin of a helper started with ReadStdin.
	stdin *os.File
}

// startHelper starts the test binary as a helper process in mode.
func startHelper(t *testing.T, mode string, args helperArgs) *helper {
	t.Helper()
	encoded, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	reportR, reportW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	resumeR, resumeW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	h := &helper{
		cmd:    exec.Command(os.Args[0], "-test.run=^TestSpoolHelper$"),
		report: bufio.NewReader(reportR), resume: resumeW, output: &strings.Builder{},
	}
	h.cmd.Env = append(os.Environ(), spoolHelperMode+"="+mode, spoolHelperArgs+"="+string(encoded))
	h.cmd.ExtraFiles = []*os.File{reportW, resumeR}
	// A group of its own: the kernel sends SIGHUP and SIGCONT to a
	// stopped process whose group becomes orphaned, which ends the stop
	// and releases the locks of the stopped helper.
	h.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	h.cmd.Stdout, h.cmd.Stderr = h.output, h.output
	var stdinR *os.File
	if args.ReadStdin {
		if stdinR, h.stdin, err = os.Pipe(); err != nil {
			t.Fatal(err)
		}
		h.cmd.Stdin = stdinR
	}
	if err := h.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = reportW.Close()
	_ = resumeR.Close()
	if stdinR != nil {
		_ = stdinR.Close()
	}
	t.Cleanup(func() {
		if h.stdin != nil {
			_ = h.stdin.Close()
		}
		_ = resumeW.Close()
		_ = reportR.Close()
		if h.cmd.ProcessState == nil {
			_ = h.cmd.Process.Kill()
			_ = h.cmd.Wait()
		}
	})
	return h
}

// readReport returns the next line the helper reports; it fails when the
// helper exits without one.
func (h *helper) readReport(t *testing.T) string {
	t.Helper()
	line, err := h.report.ReadString('\n')
	if err != nil {
		t.Fatalf("helper reported nothing: %v; output:\n%s", err, h.output.String())
	}
	return strings.TrimSuffix(line, "\n")
}

// receiver is an HTTP target that records the subject of every request
// per path; hang makes it hold requests until the client goes away.
type receiver struct {
	*httptest.Server
	mu       sync.Mutex
	got      map[string][]string
	hang     map[string]bool
	arrivals chan string
}

func newReceiver(t *testing.T) *receiver {
	t.Helper()
	r := &receiver{got: map[string][]string{}, hang: map[string]bool{}, arrivals: make(chan string, 100)}
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body struct{ Subject string }
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		r.mu.Lock()
		isHanging := r.hang[req.URL.Path]
		if !isHanging {
			r.got[req.URL.Path] = append(r.got[req.URL.Path], body.Subject)
		}
		r.mu.Unlock()
		r.arrivals <- req.URL.Path
		if isHanging {
			<-req.Context().Done()
			return
		}
	}))
	t.Cleanup(r.Close)
	return r
}

func (r *receiver) setHang(path string, isHanging bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hang[path] = isHanging
}

// delivered returns the subjects path accepted.
func (r *receiver) delivered(path string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.got[path]...)
}

// config returns a configuration with a generic-json target per name,
// each posting to /<name> on the receiver.
func (r *receiver) config(names ...string) string {
	var doc strings.Builder
	for _, name := range names {
		fmt.Fprintf(&doc, "[target.%s]\ntype = \"http\"\npreset = \"generic-json\"\nurl = \"%s/%s\"\n\n", name, r.URL, name)
	}
	return doc.String()
}

// queueEntries writes entries with subjects into queue/ of dir, all
// targets pending and due, owned by owner.
func queueEntries(t *testing.T, dir string, owner int, targets []string, subjects ...string) {
	t.Helper()
	sp, err := spool.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i, subject := range subjects {
		now := time.Now().Add(time.Duration(i) * time.Millisecond)
		e := spool.NewEntry(spool.NewID(now), owner, now, now, message.Envelope{}, targets)
		rec, err := sp.Create(spool.QueueDir, e, []byte("Subject: "+subject+"\n\nbody\n"), spool.Quota{})
		if err != nil {
			t.Fatal(err)
		}
		_ = rec.Close()
	}
}

// realInvocation is an in-process invocation against the receiver with
// the real clock and delivery.
func realInvocation(config, dir string, creds Credentials, args ...string) *invocation {
	return &invocation{config: config, spoolDir: dir, creds: creds, args: args, now: time.Now}
}

// TestSpoolCrashAfterFirstTarget kills a delivering process right after
// the sidecar records the first target: a later queue run sends only to
// the target that is left.
func TestSpoolCrashAfterFirstTarget(t *testing.T) {
	r := newReceiver(t)
	r.setHang("/b", true)
	dir := t.TempDir()
	config := r.config("a", "b")
	h := startHelper(t, helperKillAfterDone, helperArgs{Config: config, SpoolDir: dir, Creds: elevatedUser, Args: []string{"-ti"}, Stdin: "Subject: crash\n\nbody\n"})
	err := h.cmd.Wait()
	if status, ok := h.cmd.ProcessState.Sys().(syscall.WaitStatus); !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatalf("helper ended with %v, want SIGKILL; output:\n%s", err, h.output.String())
	}
	r.setHang("/b", false)
	inv := realInvocation(config, dir, serviceCaller, "-q")
	if code := inv.run(t); code != 0 {
		t.Fatalf("-q = %d; output:\n%s", code, inv.output())
	}
	if a, b := r.delivered("/a"), r.delivered("/b"); len(a) != 1 || len(b) != 1 || b[0] != "crash" {
		t.Errorf("a got %v, b got %v, want each once", a, b)
	}
	sp, _ := spool.Open(dir)
	if ids, _ := sp.List(spool.QueueDir); len(ids) != 0 {
		t.Errorf("queue/ = %v, want empty", ids)
	}
}

// TestSpoolKilledWhileReceiverHangs kills the process with SIGKILL while
// its only target hangs: the message stays queued, -q delivers it, and
// mailq shows an empty queue afterwards.
func TestSpoolKilledWhileReceiverHangs(t *testing.T) {
	r := newReceiver(t)
	r.setHang("/a", true)
	dir := t.TempDir()
	config := r.config("a")
	h := startHelper(t, helperPlain, helperArgs{Config: config, SpoolDir: dir, Creds: elevatedUser, Args: []string{"-ti"}, Stdin: "Subject: hung\n\nbody\n"})
	select {
	case <-r.arrivals:
	case <-time.After(time.Minute):
		t.Fatalf("the receiver got no request; output:\n%s", h.output.String())
	}
	if err := h.cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = h.cmd.Wait()
	r.setHang("/a", false)
	run := realInvocation(config, dir, serviceCaller, "-q")
	if code := run.run(t); code != 0 || len(r.delivered("/a")) != 1 {
		t.Fatalf("-q = %d, a got %v; output:\n%s", code, r.delivered("/a"), run.output())
	}
	list := realInvocation(config, dir, serviceCaller, "-bp")
	if code := list.run(t); code != 0 || list.stdout.String() != "queue is empty\n" {
		t.Errorf("-bp = %d, stdout %q", code, list.stdout.String())
	}
}

// TestSpoolConcurrentRuns runs two queue runs at once over the same
// entries, the second started while the first holds an entry whose first
// target is recorded as delivered: every target of every entry gets the
// message exactly once.
func TestSpoolConcurrentRuns(t *testing.T) {
	r := newReceiver(t)
	dir := t.TempDir()
	config := r.config("a", "b")
	var subjects []string
	for i := range 8 {
		subjects = append(subjects, "m"+strconv.Itoa(i))
	}
	queueEntries(t, dir, 1000, []string{"a", "b"}, subjects...)
	first := startHelper(t, helperPauseAfterDone, helperArgs{Config: config, SpoolDir: dir, Creds: serviceCaller, Args: []string{"-q"}})
	paused := first.readReport(t)
	second := startHelper(t, helperPlain, helperArgs{Config: config, SpoolDir: dir, Creds: serviceCaller, Args: []string{"-q"}})
	_ = first.resume.Close()
	for _, h := range []*helper{first, second} {
		if err := h.cmd.Wait(); err != nil {
			t.Errorf("helper failed: %v; output:\n%s", err, h.output.String())
		}
	}
	for _, path := range []string{"/a", "/b"} {
		got := r.delivered(path)
		counts := map[string]int{}
		for _, subject := range got {
			counts[subject]++
		}
		for _, subject := range subjects {
			if counts[subject] != 1 {
				t.Errorf("%s got %s %d times, want once; all: %v", path, subject, counts[subject], got)
			}
		}
	}
	sp, _ := spool.Open(dir)
	if ids, _ := sp.List(spool.QueueDir); len(ids) != 0 {
		t.Errorf("queue/ = %v, want empty; the first run paused on %s", ids, paused)
	}
}

// TestSpoolStoppedUserRun stops a user's queue run with SIGSTOP while it
// holds the user's run lock and one entry: the service user's run
// delivers every other entry, the user's own entries included.
func TestSpoolStoppedUserRun(t *testing.T) {
	r := newReceiver(t)
	dir := t.TempDir()
	config := r.config("a")
	queueEntries(t, dir, 1000, []string{"a"}, "alice-1", "alice-2")
	queueEntries(t, dir, 1001, []string{"a"}, "bob")
	h := startHelper(t, helperStopWhenLocked, helperArgs{Config: config, SpoolDir: dir, Creds: elevatedUser, Args: []string{"-q"}})
	locked := h.readReport(t)
	var status syscall.WaitStatus
	if _, err := syscall.Wait4(h.cmd.Process.Pid, &status, syscall.WUNTRACED, nil); err != nil || !status.Stopped() {
		t.Fatalf("helper not stopped: %v, status %v; output:\n%s", err, status, h.output.String())
	}
	user := realInvocation(config, dir, elevatedUser, "-q")
	if code := user.run(t); code != 0 || len(r.delivered("/a")) != 0 {
		t.Fatalf("second user run = %d, a got %v, want 0 and nothing sent; output:\n%s", code, r.delivered("/a"), user.output())
	}
	service := realInvocation(config, dir, serviceCaller, "-q")
	if code := service.run(t); code != 0 {
		t.Fatalf("-q = %d; output:\n%s", code, service.output())
	}
	got := r.delivered("/a")
	if len(got) != 2 || strings.Contains(strings.Join(got, ","), "alice-1") {
		t.Errorf("a got %v, want alice-2 and bob, alice-1 held by the stopped run", got)
	}
	sp, _ := spool.Open(dir)
	if ids, _ := sp.List(spool.QueueDir); len(ids) != 1 || ids[0] != locked {
		t.Errorf("queue/ = %v, want only %s", ids, locked)
	}
}
