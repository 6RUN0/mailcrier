package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/iotest"
	"time"

	"github.com/6RUN0/mailcrier/internal/backend"
	"github.com/6RUN0/mailcrier/internal/config"
	"github.com/6RUN0/mailcrier/internal/delivery"
	"github.com/6RUN0/mailcrier/internal/message"
	"github.com/6RUN0/mailcrier/internal/redact"
	"github.com/6RUN0/mailcrier/internal/render"
	"github.com/6RUN0/mailcrier/internal/spool"
	"github.com/6RUN0/mailcrier/internal/text"
)

// service stands in for the targets: each target answers with the
// statuses queued for it, OK once they run out, and remembers the
// subjects it was sent.
type service struct {
	mu sync.Mutex
	// replies are the statuses of the next calls, per target.
	replies map[string][]delivery.Status
	// retryAfter is the delay a temporary failure of the target asks for.
	retryAfter map[string]time.Duration
	// panicking marks the targets whose permanent failure is a panic.
	panicking map[string]bool
	// sent lists the subjects each target got, whatever the outcome.
	sent map[string][]string
	// clock, when set, advances by step on every call.
	clock *clock
	step  time.Duration
}

func newService() *service {
	return &service{
		replies: map[string][]delivery.Status{}, retryAfter: map[string]time.Duration{}, panicking: map[string]bool{},
		sent: map[string][]string{},
	}
}

// reply queues statuses for target.
func (s *service) reply(target string, statuses ...delivery.Status) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.replies[target] = append(s.replies[target], statuses...)
}

func (s *service) deliver(_ context.Context, targets []delivery.Target, _ message.Envelope, d render.Data, _ []message.Attachment) []delivery.Result {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.clock != nil {
		s.clock.advance(s.step)
	}
	var results []delivery.Result
	for _, target := range targets {
		s.sent[target.ID] = append(s.sent[target.ID], d.Subject)
		status := delivery.OK
		if queued := s.replies[target.ID]; len(queued) > 0 {
			status, s.replies[target.ID] = queued[0], queued[1:]
		}
		result := delivery.Result{TargetID: target.ID, Status: status}
		switch status {
		case delivery.Temp:
			result.Err = &backend.Error{Class: backend.Temporary, Status: 503, RetryAfter: s.retryAfter[target.ID], Err: errors.New("unavailable")}
		case delivery.Perm:
			result.Err = &backend.Error{Class: backend.Permanent, Status: 400, Err: errors.New("rejected")}
			if s.panicking[target.ID] {
				result.Err = &backend.PanicError{Value: "boom"}
			}
		}
		results = append(results, result)
	}
	return results
}

// got returns the subjects target was sent.
func (s *service) got(target string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.sent[target])
}

// clock is a settable clock for the invocations.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: testNow} }

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// spoolCase runs invocations against one spool directory, one service
// and one clock.
type spoolCase struct {
	t       *testing.T
	dir     string
	config  string
	service *service
	clock   *clock
}

func newSpoolCase(t *testing.T) *spoolCase {
	t.Helper()
	return &spoolCase{t: t, dir: t.TempDir(), config: twoTargets, service: newService(), clock: newClock()}
}

// send delivers a message with subject as creds, a millisecond after the
// previous one, and returns the exit status and the invocation.
func (c *spoolCase) send(subject string, creds Credentials) (int, *invocation) {
	c.t.Helper()
	inv := c.invocation(creds, nil)
	inv.stdin = strings.NewReader("Subject: " + subject + "\n\nbody\n")
	code := inv.run(c.t)
	// Entries sort by the time they were queued.
	c.clock.advance(time.Millisecond)
	return code, inv
}

// queueRun runs the queue with args, -q by default, as creds.
func (c *spoolCase) queueRun(creds Credentials, args ...string) (int, *invocation) {
	c.t.Helper()
	if args == nil {
		args = []string{"-q"}
	}
	inv := c.invocation(creds, args)
	return inv.run(c.t), inv
}

func (c *spoolCase) invocation(creds Credentials, args []string) *invocation {
	return &invocation{
		config: c.config, args: args, creds: creds, spoolDir: c.dir,
		deliver: c.service.deliver, now: c.clock.now,
	}
}

// ids lists the entries of area.
func (c *spoolCase) ids(area string) []string {
	c.t.Helper()
	sp, err := spool.Open(c.dir)
	if err != nil {
		c.t.Fatal(err)
	}
	ids, err := sp.List(area)
	if err != nil {
		c.t.Fatal(err)
	}
	return ids
}

// entry reads the sidecar of the only entry in area.
func (c *spoolCase) entry(area string) *spool.Entry {
	c.t.Helper()
	ids := c.ids(area)
	if len(ids) != 1 {
		c.t.Fatalf("%s/ holds %v, want one entry", area, ids)
	}
	sp, err := spool.Open(c.dir)
	if err != nil {
		c.t.Fatal(err)
	}
	e, err := sp.Peek(area, ids[0])
	if err != nil {
		c.t.Fatal(err)
	}
	return e
}

var (
	rootCaller    = Credentials{UID: 0, GID: 0, EGID: 0, ServiceUID: 990}
	serviceCaller = Credentials{UID: 990, GID: 990, EGID: 990, ServiceUID: 990}
)

// elevated returns the credentials of user uid running the setgid binary.
func elevated(uid int) Credentials {
	return Credentials{UID: uid, GID: uid, EGID: 990, ServiceUID: 990}
}

// TestSpoolDelivery covers the first delivery of a message with the spool
// on: the entry is written ahead and stays only for what is still due.
func TestSpoolDelivery(t *testing.T) {
	t.Run("T-ADJ-15/success-sends-once-and-leaves-nothing", func(t *testing.T) {
		c := newSpoolCase(t)
		code, inv := c.send("ok", elevatedUser)
		if code != 0 || !slices.Equal(c.service.got("a"), []string{"ok"}) || !slices.Equal(c.service.got("b"), []string{"ok"}) {
			t.Fatalf("Run() = %d, sent a %v, b %v; output:\n%s", code, c.service.got("a"), c.service.got("b"), inv.output())
		}
		for _, area := range []string{spool.QueueDir, spool.TmpDir, spool.HoldDir, spool.FailedDir} {
			if ids := c.ids(area); len(ids) != 0 {
				t.Errorf("%s/ = %v, want empty", area, ids)
			}
		}
	})
	t.Run("T-ADJ-16/temporary-failure-queued-and-accepted", func(t *testing.T) {
		c := newSpoolCase(t)
		c.service.reply("a", delivery.Temp)
		c.service.reply("b", delivery.Temp)
		code, inv := c.send("later", elevatedUser)
		if code != 0 {
			t.Fatalf("Run() = %d, want 0; output:\n%s", code, inv.output())
		}
		e := c.entry(spool.QueueDir)
		if e.OwnerUID != 1000 || e.Targets["a"].State != spool.Pending || e.Targets["a"].Attempts != 1 || !e.Targets["a"].NextAt.Equal(testNow.Add(time.Minute)) {
			t.Errorf("entry = %+v, a = %+v", e, e.Targets["a"])
		}
		if !strings.Contains(inv.output(), `level=WARN msg="message queued"`) {
			t.Errorf("output lacks the queued warning:\n%s", inv.output())
		}
	})
	t.Run("T-ADJ-17/permanent-failure-reported-nothing-queued", func(t *testing.T) {
		c := newSpoolCase(t)
		c.service.reply("a", delivery.Perm)
		c.service.reply("b", delivery.Perm)
		if code, inv := c.send("bad", elevatedUser); code != 69 {
			t.Fatalf("Run() = %d, want 69; output:\n%s", code, inv.output())
		}
		if ids := c.ids(spool.QueueDir); len(ids) != 0 {
			t.Errorf("queue/ = %v, want empty", ids)
		}
	})
	t.Run("partial-success-queues-the-rest", func(t *testing.T) {
		c := newSpoolCase(t)
		c.service.reply("b", delivery.Temp)
		code, inv := c.send("half", elevatedUser)
		if code != 0 || !strings.Contains(inv.output(), `msg="message queued for target" target=b`) {
			t.Fatalf("Run() = %d; output:\n%s", code, inv.output())
		}
		e := c.entry(spool.QueueDir)
		if e.Targets["a"].State != spool.Done || e.Targets["b"].State != spool.Pending {
			t.Errorf("targets = a %+v, b %+v", e.Targets["a"], e.Targets["b"])
		}
	})
	t.Run("perm-and-temp-keeps-temp", func(t *testing.T) {
		c := newSpoolCase(t)
		c.service.reply("a", delivery.Perm)
		c.service.reply("b", delivery.Temp)
		if code, inv := c.send("mixed", elevatedUser); code != 69 {
			t.Fatalf("Run() = %d, want 69; output:\n%s", code, inv.output())
		}
		e := c.entry(spool.QueueDir)
		if e.Targets["a"].State != spool.Failed || e.Targets["a"].LastClass != "perm" || e.Targets["b"].State != spool.Pending {
			t.Errorf("targets = a %+v, b %+v", e.Targets["a"], e.Targets["b"])
		}
	})
}

// TestSpoolInternalError pins that a target that failed by a panic leaves
// the message in failed/ with the reason internal error, once no other
// target waits for it, instead of removing it as a rejection does: a
// retry would repeat the bug, and mailq shows the message.
func TestSpoolInternalError(t *testing.T) {
	t.Run("own-message", func(t *testing.T) {
		c := newSpoolCase(t)
		c.service.reply("a", delivery.Perm)
		c.service.panicking["a"] = true
		if code, inv := c.send("bug", elevatedUser); code != 0 {
			t.Fatalf("Run() = %d, want 0: b took the message; output:\n%s", code, inv.output())
		}
		e := c.entry(spool.FailedDir)
		if e.Reason != reasonInternal || e.Targets["a"].State != spool.Failed || e.Targets["a"].LastClass != classInternal ||
			e.Targets["a"].LastError != "panic: boom" || e.Targets["b"].State != spool.Done {
			t.Errorf("entry = %+v, a = %+v, b = %+v", e, e.Targets["a"], e.Targets["b"])
		}
		if ids := c.ids(spool.QueueDir); len(ids) != 0 {
			t.Errorf("queue/ = %v, want empty", ids)
		}
	})
	t.Run("expiry-keeps-internal-reason", func(t *testing.T) {
		c := newSpoolCase(t)
		c.service.reply("a", delivery.Perm)
		c.service.reply("b", delivery.Temp)
		c.service.panicking["a"] = true
		c.send("bug", elevatedUser)
		c.clock.advance(7*24*time.Hour + time.Second)
		if code, inv := c.queueRun(rootCaller); code != 0 || !strings.Contains(inv.output(), `msg="message failed" id=`) {
			t.Fatalf("-q = %d; output:\n%s", code, inv.output())
		}
		if e := c.entry(spool.FailedDir); e.Reason != reasonInternal {
			t.Errorf("failed entry reason = %q, want %q", e.Reason, reasonInternal)
		}
	})
	t.Run("queue-run-waits-for-pending-target", func(t *testing.T) {
		c := newSpoolCase(t)
		c.service.reply("a", delivery.Temp, delivery.Perm)
		c.service.reply("b", delivery.Temp, delivery.Temp)
		c.service.panicking["a"] = true
		if code, inv := c.send("bug", elevatedUser); code != 0 {
			t.Fatalf("Run() = %d, want 0; output:\n%s", code, inv.output())
		}
		c.clock.advance(2 * time.Minute)
		if code, inv := c.queueRun(rootCaller); code != 0 {
			t.Fatalf("first -q = %d, want 0; output:\n%s", code, inv.output())
		}
		e := c.entry(spool.QueueDir)
		if e.Targets["a"].LastClass != classInternal || e.Targets["b"].State != spool.Pending {
			t.Fatalf("after the panic a = %+v, b = %+v; want a failed internal, b pending", e.Targets["a"], e.Targets["b"])
		}
		c.clock.advance(3 * time.Minute)
		code, inv := c.queueRun(rootCaller)
		if code != 0 || !strings.Contains(inv.output(), `msg="message failed" id=`+e.ID+` reason="internal error"`) {
			t.Fatalf("second -q = %d; output:\n%s", code, inv.output())
		}
		if got := c.entry(spool.FailedDir); got.Reason != reasonInternal || got.Targets["b"].State != spool.Done {
			t.Errorf("failed entry = %+v, b = %+v", got, got.Targets["b"])
		}
		mailq := c.invocation(rootCaller, []string{"-bp"})
		if code := mailq.run(t); code != 0 || !strings.Contains(mailq.stdout.String(), `reason="internal error"`) ||
			!strings.Contains(mailq.stdout.String(), `a failed attempts=1 internal="panic: boom"`) {
			t.Errorf("mailq = %d:\n%s", code, mailq.stdout.String())
		}
	})
	t.Run("real-panic-keeps-token-out-of-spool", func(t *testing.T) {
		dir := t.TempDir()
		inv := &invocation{
			config: httpTargetConfig("https://hooks.example.org/hook/" + secretToken), client: &http.Client{Transport: panickingTransport{}},
			spoolDir: dir, stdin: strings.NewReader("Subject: t\n\nb\n"),
		}
		if code := inv.run(t); code != 69 {
			t.Fatalf("Run() = %d, want 69; output:\n%s", code, inv.output())
		}
		files := spoolFiles(t, dir)
		var sidecar string
		for name, content := range files {
			if strings.HasPrefix(name, "/"+spool.FailedDir+"/") && strings.HasSuffix(name, ".json") {
				sidecar = content
			}
		}
		if !strings.Contains(sidecar, `"last_class": "internal"`) || !strings.Contains(sidecar, "panic: unexpected request to ***") {
			t.Errorf("failed/ sidecar = %q; spool: %v", sidecar, slices.Collect(maps.Keys(files)))
		}
		for name, content := range files {
			if strings.Contains(content, secretToken) {
				t.Errorf("%s contains the token", name)
			}
		}
		if strings.Contains(inv.output(), secretToken) {
			t.Errorf("output contains the token:\n%s", inv.output())
		}
		if want := `msg="target failed" target=hook class=perm err="panic: unexpected request to ***" stack="goroutine `; !strings.Contains(inv.output(), want) {
			t.Errorf("output lacks %q:\n%s", want, inv.output())
		}
	})
}

// panickingFS fails the way a bug in reading the configuration would.
type panickingFS struct{}

func (panickingFS) Open(string) (fs.File, error) {
	panic("bug in the file system")
}

// TestRunPanic covers a panic in the main goroutine of a call with a
// message: the record carries the value and the stack, redacted; the queue
// run after the message is skipped; and a message read but not yet in the
// spool is held with the reason internal error.
func TestRunPanic(t *testing.T) {
	t.Run("T-TPL-12/panic-record-redacted", func(t *testing.T) {
		dir := t.TempDir()
		inv := &invocation{
			config: httpTargetConfig("https://hooks.example.org/hook/" + secretToken), spoolDir: dir,
			stdin: strings.NewReader("Subject: t\n\nb\n"),
			deliver: func(context.Context, []delivery.Target, message.Envelope, render.Data, []message.Attachment) []delivery.Result {
				panic("unexpected request to https://hooks.example.org/hook/" + secretToken)
			},
		}
		if code := inv.run(t); code != 70 {
			t.Fatalf("Run() = %d, want 70; output:\n%s", code, inv.output())
		}
		output := inv.output()
		if !strings.Contains(output, `level=ERROR msg="panic, call ended" panic="unexpected request to ***" stack="goroutine `) {
			t.Errorf("output lacks the redacted panic with its stack:\n%s", output)
		}
		if strings.Contains(output, secretToken) {
			t.Errorf("output contains the token:\n%s", output)
		}
		c := &spoolCase{t: t, dir: dir}
		if queued, held := c.ids(spool.QueueDir), c.ids(spool.HoldDir); len(queued) != 1 || len(held) != 0 {
			t.Errorf("queue/ = %v, hold/ = %v, want the entry written ahead and nothing held", queued, held)
		}
	})
	t.Run("queue-run-skipped", func(t *testing.T) {
		c := newSpoolCase(t)
		c.service.reply("a", delivery.Temp)
		c.send("old", elevatedUser)
		c.clock.advance(time.Minute)
		inv := c.invocation(elevatedUser, nil)
		inv.stdin = strings.NewReader("Subject: new\n\nbody\n")
		inv.deliver = func(ctx context.Context, targets []delivery.Target, env message.Envelope, d render.Data, files []message.Attachment) []delivery.Result {
			if d.Subject == "new" {
				panic("bug in the delivery")
			}
			return c.service.deliver(ctx, targets, env, d, files)
		}
		if code := inv.run(t); code != 70 {
			t.Fatalf("Run() = %d, want 70; output:\n%s", code, inv.output())
		}
		if got := c.service.got("a"); !slices.Equal(got, []string{"old"}) {
			t.Errorf("a got %v, want the queued message untouched by the call that panicked", got)
		}
	})
	t.Run("callback-panic-raised-in-caller", func(t *testing.T) {
		inv := &invocation{
			config: httpTargetConfig(newCountingServer(t, false).URL), spoolDir: t.TempDir(),
			stdin:      strings.NewReader("Subject: t\n\nb\n"),
			spoolSaved: func(*spool.Entry) { panic("bug in the spool hook") },
		}
		if code := inv.run(t); code != 70 {
			t.Fatalf("Run() = %d, want 70; output:\n%s", code, inv.output())
		}
		if want := `msg="panic, call ended" panic="bug in the spool hook" stack="goroutine `; !strings.Contains(inv.output(), want) ||
			!strings.Contains(inv.output(), "TestRunPanic") {
			t.Errorf("output lacks %q with the stack of the callback:\n%s", want, inv.output())
		}
	})
	t.Run("message-held-right-after-reading", func(t *testing.T) {
		c := newSpoolCase(t)
		inv := c.invocation(elevatedUser, nil)
		inv.stdin = strings.NewReader("Subject: kept\n\nbody\n")
		inv.catchSignals = func(context.Context) (context.Context, context.CancelFunc) { panic("bug before the envelope") }
		if code := inv.run(t); code != 70 {
			t.Fatalf("Run() = %d, want 70; output:\n%s", code, inv.output())
		}
		if e := c.entry(spool.HoldDir); e.Reason != reasonInternal {
			t.Errorf("held entry = %+v, want reason internal error", e)
		}
	})
	t.Run("panic-while-handling-a-panic", func(t *testing.T) {
		c := newSpoolCase(t)
		inv := c.invocation(elevatedUser, nil)
		inv.stdin = strings.NewReader("Subject: t\n\nbody\n")
		inv.deliver = func(context.Context, []delivery.Target, message.Envelope, render.Data, []message.Attachment) []delivery.Result {
			// A nil *backend.PanicError makes the record of the panic
			// panic once more.
			panic((*backend.PanicError)(nil))
		}
		if code := inv.run(t); code != 70 {
			t.Fatalf("Run() = %d, want 70; output:\n%s", code, inv.output())
		}
		if !strings.Contains(inv.output(), `level=ERROR msg="panic while handling a panic"`) {
			t.Errorf("output lacks the second panic:\n%s", inv.output())
		}
	})
	t.Run("message-held", func(t *testing.T) {
		c := newSpoolCase(t)
		inv := c.invocation(elevatedUser, nil)
		inv.configFS = panickingFS{}
		inv.stdin = strings.NewReader("Subject: kept\n\nbody\n")
		if code := inv.run(t); code != 70 {
			t.Fatalf("Run() = %d, want 70; output:\n%s", code, inv.output())
		}
		if !strings.Contains(inv.output(), `panic="bug in the file system"`) || !strings.Contains(inv.output(), `level=WARN msg="message held"`) {
			t.Errorf("output lacks the panic and the held message:\n%s", inv.output())
		}
		if e := c.entry(spool.HoldDir); e.Reason != reasonInternal {
			t.Errorf("held entry = %+v, want reason internal error", e)
		}
		if code, inv := c.queueRun(rootCaller); code != 0 || len(c.ids(spool.HoldDir)) != 0 || !slices.Equal(c.service.got("a"), []string{"kept"}) {
			t.Errorf("-q = %d, hold/ %v, a got %v; output:\n%s", code, c.ids(spool.HoldDir), c.service.got("a"), inv.output())
		}
	})
}

// statusServer answers every request with the status in status.
func statusServer(t *testing.T, status *atomic.Int32) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(int(status.Load()))
	}))
	t.Cleanup(server.Close)
	return server
}

// TestSpoolCallbackPanic pins that a panic while one result is recorded
// loses no result: every target is logged and recorded, and the entry
// keeps the target still pending instead of going to failed/.
func TestSpoolCallbackPanic(t *testing.T) {
	var statusA, statusB atomic.Int32
	statusA.Store(http.StatusOK)
	statusB.Store(http.StatusServiceUnavailable)
	config := "[target.a]\ntype = \"http\"\npreset = \"generic-json\"\nurl = \"" + statusServer(t, &statusA).URL + "\"\n\n" +
		"[target.b]\ntype = \"http\"\npreset = \"generic-json\"\nurl = \"" + statusServer(t, &statusB).URL + "\"\n"
	// panicOnce makes the first write of a sidecar panic, as a bug in
	// recording one result would; recording it again succeeds.
	panicOnce := func() func(*spool.Entry) {
		var calls atomic.Int32
		return func(*spool.Entry) {
			if calls.Add(1) == 1 {
				panic("bug in recording")
			}
		}
	}
	checkPending := func(t *testing.T, dir string) {
		t.Helper()
		c := &spoolCase{t: t, dir: dir}
		if failed := c.ids(spool.FailedDir); len(failed) != 0 {
			t.Errorf("failed/ = %v, want empty", failed)
		}
		if e := c.entry(spool.QueueDir); e.Targets["a"].State != spool.Done || e.Targets["b"].State != spool.Pending {
			t.Errorf("targets a %+v, b %+v; want a done, b pending", e.Targets["a"], e.Targets["b"])
		}
	}
	t.Run("own-message", func(t *testing.T) {
		dir := t.TempDir()
		inv := &invocation{config: config, spoolDir: dir, stdin: strings.NewReader("Subject: t\n\nb\n"), spoolSaved: panicOnce()}
		if code := inv.run(t); code != 70 {
			t.Fatalf("Run() = %d, want 70; output:\n%s", code, inv.output())
		}
		for _, want := range []string{
			`msg="target failed" target=b class=temp status=503`, `msg="message queued for target" target=b`,
			`msg="panic, call ended" panic="bug in recording" stack="goroutine `,
		} {
			if !strings.Contains(inv.output(), want) {
				t.Errorf("output lacks %q:\n%s", want, inv.output())
			}
		}
		checkPending(t, dir)
	})
	t.Run("queue-run", func(t *testing.T) {
		dir := t.TempDir()
		statusA.Store(http.StatusServiceUnavailable)
		first := &invocation{config: config, spoolDir: dir, stdin: strings.NewReader("Subject: t\n\nb\n")}
		if code := first.run(t); code != 0 {
			t.Fatalf("Run() = %d, want 0; output:\n%s", code, first.output())
		}
		statusA.Store(http.StatusOK)
		inv := &invocation{
			config: config, spoolDir: dir, args: []string{"-q"}, creds: rootCaller, spoolSaved: panicOnce(),
			now: func() time.Time { return testNow.Add(2 * time.Minute) },
		}
		if code := inv.run(t); code != 70 {
			t.Fatalf("-q = %d, want 70; output:\n%s", code, inv.output())
		}
		for _, want := range []string{`target=b class=temp status=503`, `msg="panic in queue run" id=`} {
			if !strings.Contains(inv.output(), want) {
				t.Errorf("output lacks %q:\n%s", want, inv.output())
			}
		}
		checkPending(t, dir)
	})
}

// textOnceErr is an error whose text panics the first time it is read, as
// a bug in recording a result would; fmt would recover it inside a
// *backend.Error, so a sender returns it bare.
type textOnceErr struct{ reads *atomic.Int32 }

func (e textOnceErr) Error() string {
	if e.reads.Add(1) == 1 {
		panic("bug in the error text")
	}
	return "unavailable"
}

// errSender fails every send with err, nil for none.
type errSender struct{ err error }

func (errSender) Caps() backend.Caps { return backend.Caps{} }

func (s errSender) Send(context.Context, backend.Payload) error { return s.err }

// TestSendRecordsAfterCallbackPanic pins that a result whose recording
// panicked in the goroutine of its target is recorded once more after the
// targets finished: target b, rejected, ends failed, and the entry, which
// no target waits for any more, is removed.
func TestSendRecordsAfterCallbackPanic(t *testing.T) {
	settings := config.DefaultSpool()
	settings.Dir = t.TempDir()
	tmpl, err := render.Builtin(text.FormatPlain)
	if err != nil {
		t.Fatal(err)
	}
	targets := []delivery.Target{
		{ID: "a", Sender: errSender{}, Template: tmpl},
		{ID: "b", Sender: errSender{err: textOnceErr{reads: &atomic.Int32{}}}, Template: tmpl},
	}
	var logs strings.Builder
	q, err := newQueue(Deps{Now: func() time.Time { return testNow }}, slog.New(slog.NewTextHandler(&logs, nil)), &redact.Redactor{}, settings, targets)
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte("Subject: t\n\nbody\n")
	msg, bcc, _, err := message.Read(bytes.NewReader(raw), message.ReadOptions{MaxSize: message.MaxSize, ReceivedAt: testNow})
	if err != nil {
		t.Fatal(err)
	}
	entry := spool.NewEntry(spool.NewID(testNow), 1000, testNow, testNow, message.Envelope{}, []string{"a", "b"})
	rec, err := q.sp.Create(spool.QueueDir, entry, msg.Raw, spool.Quota{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rec.Close() }()
	data := render.NewData(msg, message.Envelope{}, bcc)
	data.Strings = render.DefaultStrings()
	_, donePanic := q.send(context.Background(), targets, message.Envelope{}, data, nil, msg.Raw, rec)
	if donePanic == nil || donePanic.Value != "bug in the error text" {
		t.Fatalf("send() panic = %#v, want the panic of recording b", donePanic)
	}
	if b := rec.Entry.Targets["b"]; b.State != spool.Failed || b.LastError != "unavailable" || q.sp.Has(spool.QueueDir, rec.ID()) {
		t.Errorf("b = %+v, entry in queue/ %v; want b failed and the entry removed", b, q.sp.Has(spool.QueueDir, rec.ID()))
	}
}

// TestSpoolNotAvailable covers the exit statuses when the entry cannot be
// written: the message is delivered directly, and a temporary failure is
// then lost and reported.
func TestSpoolNotAvailable(t *testing.T) {
	t.Run("T-MTA-36/missing-directory-exits-73", func(t *testing.T) {
		c := newSpoolCase(t)
		c.dir = filepath.Join(c.dir, "missing")
		c.service.reply("a", delivery.Temp)
		c.service.reply("b", delivery.Temp)
		code, inv := c.send("lost", elevatedUser)
		if code != 73 || len(c.service.got("a")) != 1 {
			t.Fatalf("Run() = %d, sent %v, want 73 after a direct attempt; output:\n%s", code, c.service.got("a"), inv.output())
		}
		for _, want := range []string{`level=ERROR msg="spool entry not written"`, `msg="message lost"`} {
			if !strings.Contains(inv.output(), want) {
				t.Errorf("output lacks %s:\n%s", want, inv.output())
			}
		}
	})
	t.Run("missing-directory-delivered-exits-0", func(t *testing.T) {
		c := newSpoolCase(t)
		c.dir = filepath.Join(c.dir, "missing")
		if code, inv := c.send("fine", elevatedUser); code != 0 || !strings.Contains(inv.output(), `level=WARN msg="spool entry not written"`) {
			t.Fatalf("Run() = %d, want 0 and a warning; output:\n%s", code, inv.output())
		}
	})
	t.Run("T-MTA-36/unwritable-queue-exits-74", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root ignores directory modes")
		}
		c := newSpoolCase(t)
		if _, err := spool.Open(c.dir); err != nil {
			t.Fatal(err)
		}
		queueDir := filepath.Join(c.dir, spool.QueueDir)
		if err := os.Chmod(queueDir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(queueDir, 0o770) })
		c.service.reply("a", delivery.Temp)
		if code, inv := c.send("lost", elevatedUser); code != 74 {
			t.Fatalf("Run() = %d, want 74; output:\n%s", code, inv.output())
		}
	})
	t.Run("spool-off-all-temp-exits-69", func(t *testing.T) {
		c := newSpoolCase(t)
		c.config = "[spool]\ndir = \"\"\n\n" + twoTargets
		c.service.reply("a", delivery.Temp)
		c.service.reply("b", delivery.Temp)
		code, inv := c.send("lost", elevatedUser)
		if code != 69 || !strings.Contains(inv.output(), `msg="message lost"`) {
			t.Fatalf("Run() = %d, want 69 and message lost; output:\n%s", code, inv.output())
		}
		if ids := c.ids(spool.QueueDir); len(ids) != 0 {
			t.Errorf("queue/ = %v with the spool off", ids)
		}
	})
}

// TestSpoolNotWritable pins that --status and -q report a spool that
// takes no new entry: --status prints its counts and exits 74, -q exits
// 74 without a run, which could deliver entries it cannot record.
func TestSpoolNotWritable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory modes")
	}
	c := newSpoolCase(t)
	c.service.reply("a", delivery.Temp)
	c.send("s", elevatedUser)
	queueDir := filepath.Join(c.dir, spool.QueueDir)
	if err := os.Chmod(queueDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(queueDir, 0o770) })
	c.clock.advance(time.Hour)
	want := `msg="spool not writable" err="access ` + queueDir + `: permission denied"`
	code, inv := c.queueRun(serviceCaller, "--status")
	if code != 74 || !strings.HasPrefix(inv.stdout.String(), "queued=1 ") || !strings.Contains(inv.output(), `level=WARN `+want) {
		t.Errorf("--status = %d, stdout %q, want 74 and the counts; output:\n%s", code, inv.stdout.String(), inv.output())
	}
	want = `level=ERROR msg="spool not writable, queue not run" err="access ` + queueDir + `: permission denied"`
	if code, inv := c.queueRun(serviceCaller); code != 74 || !strings.Contains(inv.output(), want) || len(c.service.got("a")) != 1 {
		t.Errorf("-q = %d, a got %v, want 74 and no run; output:\n%s", code, c.service.got("a"), inv.output())
	}
}

// TestQueueRun covers -q over entries queued by earlier calls.
func TestQueueRun(t *testing.T) {
	t.Run("T-ADJ-18/due-entries-delivered-others-wait", func(t *testing.T) {
		c := newSpoolCase(t)
		c.service.reply("a", delivery.Temp)
		c.send("first", elevatedUser)
		c.clock.advance(30 * time.Second)
		c.service.reply("a", delivery.Temp)
		c.send("second", elevatedUser)
		c.clock.advance(31 * time.Second)
		if code, inv := c.queueRun(serviceCaller); code != 0 {
			t.Fatalf("-q = %d; output:\n%s", code, inv.output())
		}
		if got := c.service.got("a"); !slices.Equal(got, []string{"first", "second", "first"}) {
			t.Errorf("a got %v, want first retried after its 60 s and second left waiting", got)
		}
		if ids := c.ids(spool.QueueDir); len(ids) != 1 {
			t.Errorf("queue/ = %v, want second only", ids)
		}
	})
	t.Run("T-ADJ-20/temporary-then-success-empties-queue", func(t *testing.T) {
		c := newSpoolCase(t)
		c.service.reply("a", delivery.Temp)
		c.send("retry", elevatedUser)
		c.clock.advance(time.Minute)
		c.queueRun(serviceCaller)
		if ids := c.ids(spool.QueueDir); len(ids) != 0 || !slices.Equal(c.service.got("b"), []string{"retry"}) {
			t.Errorf("queue/ = %v, b got %v, want empty and b sent once", ids, c.service.got("b"))
		}
	})
	t.Run("T-MTA-34/q-with-interval-runs-once", func(t *testing.T) {
		c := newSpoolCase(t)
		c.service.reply("a", delivery.Temp)
		c.send("retry", elevatedUser)
		c.clock.advance(time.Minute)
		if code, inv := c.queueRun(rootCaller, "-q30m"); code != 0 || len(c.ids(spool.QueueDir)) != 0 {
			t.Fatalf("-q30m = %d, queue %v; output:\n%s", code, c.ids(spool.QueueDir), inv.output())
		}
	})
	t.Run("T-ADJ-53/run-goes-on-after-a-failure", func(t *testing.T) {
		c := newSpoolCase(t)
		for _, subject := range []string{"one", "two", "three"} {
			c.service.reply("a", delivery.Temp)
			c.send(subject, elevatedUser)
		}
		c.clock.advance(time.Minute)
		c.service.reply("a", delivery.Perm, delivery.Temp)
		if code, inv := c.queueRun(serviceCaller); code != 0 {
			t.Fatalf("-q = %d; output:\n%s", code, inv.output())
		}
		if got := c.service.got("a")[3:]; !slices.Equal(got, []string{"one", "two", "three"}) {
			t.Errorf("a got %v in the run, want all three", got)
		}
		if ids := c.ids(spool.QueueDir); len(ids) != 1 {
			t.Errorf("queue/ = %v, want only the temporary failure", ids)
		}
	})
	t.Run("T-ADJ-53/run-goes-on-after-a-panic", func(t *testing.T) {
		c := newSpoolCase(t)
		for _, subject := range []string{"one", "two"} {
			c.service.reply("a", delivery.Temp)
			c.send(subject, elevatedUser)
		}
		poisoned := c.ids(spool.QueueDir)[0]
		c.clock.advance(time.Minute)
		inv := c.invocation(serviceCaller, []string{"-q"})
		inv.deliver = func(ctx context.Context, targets []delivery.Target, env message.Envelope, d render.Data, files []message.Attachment) []delivery.Result {
			if d.Subject == "one" {
				panic("bug in the queue run")
			}
			return c.service.deliver(ctx, targets, env, d, files)
		}
		if code := inv.run(t); code != 70 {
			t.Fatalf("-q = %d, want 70; output:\n%s", code, inv.output())
		}
		output := inv.output()
		if !strings.Contains(output, `level=ERROR msg="panic in queue run" id=`+poisoned+` panic="bug in the queue run" stack="goroutine `) ||
			!strings.Contains(output, `msg="message failed" id=`+poisoned+` reason="internal error"`) {
			t.Errorf("output lacks the panic and the failed entry:\n%s", output)
		}
		if got := c.service.got("a")[2:]; !slices.Equal(got, []string{"two"}) {
			t.Errorf("a got %v in the run, want the entry after the panic", got)
		}
		if ids := c.ids(spool.QueueDir); len(ids) != 0 {
			t.Errorf("queue/ = %v, want empty", ids)
		}
		if e := c.entry(spool.FailedDir); e.ID != poisoned || e.Reason != reasonInternal {
			t.Errorf("failed entry = %+v, want %s with reason internal error", e, poisoned)
		}
	})
	t.Run("T-ADJ-53/run-goes-on-after-a-panic-in-hold", func(t *testing.T) {
		c := newSpoolCase(t)
		c.service.reply("a", delivery.Temp)
		c.send("queued", elevatedUser)
		// A rejected configuration holds the message and runs no queue.
		c.config = "[target.x]\ntype = \"nosuch\"\n"
		c.send("held", elevatedUser)
		c.config = twoTargets
		held := c.ids(spool.HoldDir)
		c.clock.advance(time.Minute)
		// The clock panics once, at its first reading after the held
		// entry is locked: release reads it first.
		var isArmed atomic.Bool
		inv := c.invocation(rootCaller, []string{"-q"})
		inv.entryLocked = func(id string) { isArmed.Store(len(held) == 1 && id == held[0]) }
		inv.now = func() time.Time {
			if isArmed.CompareAndSwap(true, false) {
				panic("bug in routing")
			}
			return c.clock.now()
		}
		if code := inv.run(t); code != 70 {
			t.Fatalf("-q = %d, want 70; output:\n%s", code, inv.output())
		}
		if !strings.Contains(inv.output(), `msg="panic in queue run" id=`+held[0]+` panic="bug in routing"`) {
			t.Errorf("output lacks the panic of the held entry:\n%s", inv.output())
		}
		if e := c.entry(spool.FailedDir); e.ID != held[0] || e.Reason != reasonInternal {
			t.Errorf("failed entry = %+v, want %v with reason internal error", e, held)
		}
		if ids := c.ids(spool.QueueDir); len(ids) != 0 || !slices.Equal(c.service.got("a"), []string{"queued", "queued"}) {
			t.Errorf("queue/ = %v, a got %v; want the queued entry delivered after the panic", ids, c.service.got("a"))
		}
	})
	t.Run("T-ADJ-19/expired-entry-moves-to-failed", func(t *testing.T) {
		c := newSpoolCase(t)
		c.service.reply("a", delivery.Temp)
		c.send("old", elevatedUser)
		c.clock.advance(7*24*time.Hour + time.Second)
		code, inv := c.queueRun(serviceCaller)
		if code != 0 || len(c.ids(spool.QueueDir)) != 0 || len(c.ids(spool.FailedDir)) != 1 {
			t.Fatalf("-q = %d, queue %v, failed %v; output:\n%s", code, c.ids(spool.QueueDir), c.ids(spool.FailedDir), inv.output())
		}
		if !strings.Contains(inv.output(), `level=ERROR msg="message failed"`) || len(c.service.got("a")) != 1 {
			t.Errorf("sent %v; output:\n%s", c.service.got("a"), inv.output())
		}
		if e := c.entry(spool.FailedDir); e.Reason != reasonExpired || !e.FailedAt.Equal(c.clock.now()) {
			t.Errorf("failed entry = %+v", e)
		}
	})
	t.Run("failed-entry-deleted-after-failed-ttl", func(t *testing.T) {
		c := newSpoolCase(t)
		c.service.reply("a", delivery.Temp)
		c.send("old", elevatedUser)
		c.clock.advance(8 * 24 * time.Hour)
		c.queueRun(serviceCaller)
		c.clock.advance(30 * 24 * time.Hour)
		if len(c.ids(spool.FailedDir)) != 1 {
			t.Fatal("deleted before failed_ttl")
		}
		c.clock.advance(time.Second)
		c.queueRun(serviceCaller)
		if ids := c.ids(spool.FailedDir); len(ids) != 0 {
			t.Errorf("failed/ = %v, want empty after failed_ttl", ids)
		}
	})
	t.Run("stale-tmp-file-removed", func(t *testing.T) {
		c := newSpoolCase(t)
		if _, err := spool.Open(c.dir); err != nil {
			t.Fatal(err)
		}
		for name, age := range map[string]time.Duration{"1-00000000000000aa.eml": time.Hour + time.Second, "1-00000000000000bb.eml": time.Hour} {
			path := filepath.Join(c.dir, spool.TmpDir, name)
			if err := os.WriteFile(path, []byte("x"), 0o660); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(path, testNow.Add(-age), testNow.Add(-age)); err != nil {
				t.Fatal(err)
			}
		}
		c.queueRun(serviceCaller)
		if ids := c.ids(spool.TmpDir); !slices.Equal(ids, []string{"1-00000000000000bb"}) {
			t.Errorf("tmp/ = %v, want the file older than 1 h removed", ids)
		}
	})
	t.Run("target-removed-from-configuration", func(t *testing.T) {
		c := newSpoolCase(t)
		c.service.reply("b", delivery.Temp)
		c.send("renamed", elevatedUser)
		c.config = strings.Replace(twoTargets, "[target.b]", "[target.c]", 1)
		c.clock.advance(time.Minute)
		code, inv := c.queueRun(serviceCaller)
		if code != 0 || len(c.ids(spool.QueueDir)) != 0 || len(c.service.got("c")) != 0 {
			t.Fatalf("-q = %d, queue %v, c got %v; output:\n%s", code, c.ids(spool.QueueDir), c.service.got("c"), inv.output())
		}
		if !strings.Contains(inv.output(), `level=ERROR msg="target removed, message dropped for it" id=`) {
			t.Errorf("output:\n%s", inv.output())
		}
	})
	t.Run("entry-of-another-version-left-alone", func(t *testing.T) {
		c := newSpoolCase(t)
		c.service.reply("a", delivery.Temp)
		c.send("future", elevatedUser)
		id := c.ids(spool.QueueDir)[0]
		path := filepath.Join(c.dir, spool.QueueDir, id+".json")
		if err := os.WriteFile(path, []byte(`{"version":2}`), 0o660); err != nil {
			t.Fatal(err)
		}
		c.clock.advance(time.Hour)
		code, inv := c.queueRun(serviceCaller)
		if code != 0 || !strings.Contains(inv.output(), `level=WARN msg="spool entry of another version left alone" id=`+id) {
			t.Fatalf("-q = %d; output:\n%s", code, inv.output())
		}
		if data, _ := os.ReadFile(path); string(data) != `{"version":2}` || len(c.service.got("a")) != 1 {
			t.Errorf("sidecar %s, a got %v", data, c.service.got("a"))
		}
	})
	t.Run("T-MTA-34/unreadable-spool-exits-74", func(t *testing.T) {
		c := newSpoolCase(t)
		c.dir = filepath.Join(c.dir, "missing")
		if code, inv := c.queueRun(serviceCaller); code != 74 {
			t.Fatalf("-q = %d, want 74; output:\n%s", code, inv.output())
		}
	})
	t.Run("message-without-sidecar-removed-and-logged", func(t *testing.T) {
		c := newSpoolCase(t)
		c.service.reply("a", delivery.Temp)
		c.send("s", elevatedUser)
		id := c.ids(spool.QueueDir)[0]
		if err := os.Remove(filepath.Join(c.dir, spool.QueueDir, id+".json")); err != nil {
			t.Fatal(err)
		}
		code, inv := c.queueRun(serviceCaller)
		if code != 0 || !strings.Contains(inv.output(), `level=INFO msg="message without sidecar removed" id=`+id+` area=queue`) || len(c.ids(spool.QueueDir)) != 0 {
			t.Errorf("-q = %d, queue %v, want the file removed and logged; output:\n%s", code, c.ids(spool.QueueDir), inv.output())
		}
	})
	t.Run("rejected-configuration-without-spool-exits-78", func(t *testing.T) {
		c := newSpoolCase(t)
		c.dir = filepath.Join(c.dir, "missing")
		c.config = "[target.a]\ntype = \"http\"\n"
		if code, inv := c.queueRun(serviceCaller); code != 78 {
			t.Fatalf("-q = %d, want 78 for the configuration first; output:\n%s", code, inv.output())
		}
	})
}

// TestQueueRunClock covers what depends on time, with the clock replaced.
func TestQueueRunClock(t *testing.T) {
	t.Run("backoff-doubles", func(t *testing.T) {
		c := newSpoolCase(t)
		c.service.reply("a", delivery.Temp, delivery.Temp)
		c.send("slow", elevatedUser)
		c.clock.advance(time.Minute)
		c.queueRun(serviceCaller)
		if next := c.entry(spool.QueueDir).Targets["a"].NextAt; !next.Equal(c.clock.now().Add(2 * time.Minute)) {
			t.Fatalf("next attempt at %v, want 2 min after the second failure", next)
		}
		c.clock.advance(2*time.Minute - time.Second)
		c.queueRun(serviceCaller)
		if n := len(c.service.got("a")); n != 2 {
			t.Fatalf("a got %d attempts before the backoff ran out, want 2", n)
		}
		c.clock.advance(time.Second)
		c.queueRun(serviceCaller)
		if n := len(c.service.got("a")); n != 3 || len(c.ids(spool.QueueDir)) != 0 {
			t.Errorf("a got %d attempts, queue %v, want delivered at the third", n, c.ids(spool.QueueDir))
		}
	})
	t.Run("retry-after-postpones-and-throttles-the-run", func(t *testing.T) {
		c := newSpoolCase(t)
		c.service.retryAfter["a"] = 10 * time.Minute
		c.service.reply("a", delivery.Temp, delivery.Temp)
		c.send("one", elevatedUser)
		c.send("two", elevatedUser)
		if next := c.entry2(spool.QueueDir, 0).Targets["a"].NextAt; !next.Equal(testNow.Add(10 * time.Minute)) {
			t.Fatalf("next attempt at %v, want the 10 min Retry-After", next)
		}
		c.clock.advance(10 * time.Minute)
		c.service.reply("a", delivery.Temp)
		c.queueRun(serviceCaller)
		if got := c.service.got("a"); !slices.Equal(got, []string{"one", "two", "one"}) {
			t.Errorf("a got %v, want two skipped in the run after one was told to wait", got)
		}
	})
	t.Run("T-ADJ-23/run-budget-ends-the-run", func(t *testing.T) {
		c := newSpoolCase(t)
		for _, subject := range []string{"one", "two", "three", "four"} {
			c.service.reply("a", delivery.Temp)
			c.send(subject, elevatedUser)
		}
		c.clock.advance(time.Minute)
		c.service.clock, c.service.step = c.clock, 25*time.Second
		code, inv := c.queueRun(serviceCaller)
		if code != 0 || !strings.Contains(inv.output(), `msg="queue run budget spent"`) {
			t.Fatalf("-q = %d; output:\n%s", code, inv.output())
		}
		if got := c.service.got("a")[4:]; !slices.Equal(got, []string{"one", "two", "three"}) {
			t.Errorf("a got %v in the run, want three entries within 60 s", got)
		}
	})
	t.Run("drain-after-own-message", func(t *testing.T) {
		c := newSpoolCase(t)
		c.config = "[spool]\ndrain_max_messages = 2\n\n" + twoTargets
		for _, subject := range []string{"one", "two", "three"} {
			c.service.reply("a", delivery.Temp)
			c.send(subject, elevatedUser)
		}
		c.service.reply("a", delivery.Temp)
		c.send("other user", elevated(1001))
		c.clock.advance(time.Minute)
		c.send("four", elevatedUser)
		if got := c.service.got("a")[4:]; !slices.Equal(got, []string{"four", "one", "two"}) {
			t.Errorf("a got %v, want the own message, then two of the caller's entries", got)
		}
	})
	t.Run("drain-budget", func(t *testing.T) {
		c := newSpoolCase(t)
		for _, subject := range []string{"one", "two", "three"} {
			c.service.reply("a", delivery.Temp)
			c.send(subject, elevatedUser)
		}
		c.clock.advance(time.Minute)
		c.service.clock, c.service.step = c.clock, 6*time.Second
		c.send("four", elevatedUser)
		if got := c.service.got("a")[3:]; !slices.Equal(got, []string{"four", "one", "two"}) {
			t.Errorf("a got %v, want the drain stopped after 10 s", got)
		}
	})
	t.Run("drain-skips-entry-not-updated", func(t *testing.T) {
		c := newSpoolCase(t)
		c.service.reply("a", delivery.Temp, delivery.Temp)
		inv := c.invocation(elevatedUser, nil)
		inv.stdin = strings.NewReader("Subject: s\n\nbody\n")
		inv.deliver = func(ctx context.Context, targets []delivery.Target, env message.Envelope, d render.Data, files []message.Attachment) []delivery.Result {
			// A directory where Save writes the new sidecar fails it.
			next := filepath.Join(c.dir, spool.TmpDir, c.ids(spool.QueueDir)[0]+".next.json")
			if err := os.MkdirAll(filepath.Join(next, "keep"), 0o700); err != nil {
				t.Fatal(err)
			}
			return c.service.deliver(ctx, targets, env, d, files)
		}
		code := inv.run(t)
		if code != 0 || !strings.Contains(inv.output(), `level=ERROR msg="spool entry not updated"`) ||
			!strings.Contains(inv.output(), `level=DEBUG msg="spool entry skipped, state not recorded"`) {
			t.Errorf("Run() = %d; output:\n%s", code, inv.output())
		}
		if got := c.service.got("a"); len(got) != 1 {
			t.Errorf("a got %v, want one attempt in the call", got)
		}
	})
	t.Run("drain-lock-error-logged", func(t *testing.T) {
		c := newSpoolCase(t)
		if err := os.WriteFile(filepath.Join(c.dir, spool.LocksDir), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		code, inv := c.send("s", elevatedUser)
		if code != 0 || !strings.Contains(inv.output(), `level=WARN msg="queue run lock not taken" err=`) {
			t.Errorf("Run() = %d, want 0 and the lock error logged; output:\n%s", code, inv.output())
		}
	})
}

// entry2 reads the sidecar of the i-th entry in area.
func (c *spoolCase) entry2(area string, i int) *spool.Entry {
	c.t.Helper()
	sp, err := spool.Open(c.dir)
	if err != nil {
		c.t.Fatal(err)
	}
	e, err := sp.Peek(area, c.ids(area)[i])
	if err != nil {
		c.t.Fatal(err)
	}
	return e
}

// TestQueueOwners covers who runs which entries: a user elevated by the
// setgid bit only their own, root and the service user all of them.
func TestQueueOwners(t *testing.T) {
	t.Run("user-runs-own-entries-only", func(t *testing.T) {
		c := newSpoolCase(t)
		c.service.reply("a", delivery.Temp, delivery.Temp)
		c.send("alice", elevatedUser)
		c.send("bob", elevated(1001))
		c.clock.advance(time.Minute)
		c.queueRun(elevated(1001))
		if got := c.service.got("a")[2:]; !slices.Equal(got, []string{"bob"}) {
			t.Errorf("a got %v, want only bob's entry", got)
		}
	})
	t.Run("owner-kept-after-rewrite-by-service-user", func(t *testing.T) {
		c := newSpoolCase(t)
		c.service.reply("a", delivery.Temp, delivery.Temp)
		c.send("alice", elevatedUser)
		c.clock.advance(time.Minute)
		c.queueRun(serviceCaller)
		if e := c.entry(spool.QueueDir); e.OwnerUID != 1000 || e.Targets["a"].Attempts != 2 {
			t.Fatalf("entry after the service run = owner %d, a %+v", e.OwnerUID, e.Targets["a"])
		}
		c.clock.advance(time.Hour)
		c.queueRun(elevated(1001))
		if got := c.service.got("a"); len(got) != 2 {
			t.Errorf("a got %v, want nothing from bob's run", got)
		}
	})
	t.Run("unelevated-caller-runs-all", func(t *testing.T) {
		c := newSpoolCase(t)
		c.service.reply("a", delivery.Temp, delivery.Temp)
		c.send("alice", elevatedUser)
		c.send("bob", elevated(1001))
		c.clock.advance(time.Minute)
		c.queueRun(plainUser)
		if ids := c.ids(spool.QueueDir); len(ids) != 0 {
			t.Errorf("queue/ = %v, want every entry delivered", ids)
		}
	})
	t.Run("user-run-lock-held", func(t *testing.T) {
		c := newSpoolCase(t)
		c.service.reply("a", delivery.Temp)
		c.send("alice", elevatedUser)
		sp, err := spool.Open(c.dir)
		if err != nil {
			t.Fatal(err)
		}
		lock, err := sp.LockRun(1000)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = lock.Close() }()
		c.clock.advance(time.Minute)
		code, inv := c.queueRun(elevatedUser)
		if code != 0 || len(c.service.got("a")) != 1 || !strings.Contains(inv.output(), `msg="queue run of this user under way"`) {
			t.Errorf("-q = %d, a got %v; output:\n%s", code, c.service.got("a"), inv.output())
		}
		c.queueRun(serviceCaller)
		if ids := c.ids(spool.QueueDir); len(ids) != 0 {
			t.Errorf("queue/ = %v, want the service run to ignore the user's run lock", ids)
		}
	})
}

// TestQueueQuota covers the limits of the spool.
func TestQueueQuota(t *testing.T) {
	const limits = "[spool]\nmax_queue_messages = 10\nmax_queue_messages_per_uid = 2\n\n"
	t.Run("user-quota-exhausted", func(t *testing.T) {
		c := newSpoolCase(t)
		c.config = limits + twoTargets
		c.service.reply("a", delivery.Temp, delivery.Temp, delivery.Temp, delivery.Temp)
		c.send("bob", elevated(1001))
		c.send("one", elevatedUser)
		c.send("two", elevatedUser)
		before := c.ids(spool.QueueDir)
		code, inv := c.send("three", elevatedUser)
		if code != 73 || !strings.Contains(inv.output(), "limit max_queue_messages_per_uid reached") {
			t.Fatalf("Run() = %d, want 73; output:\n%s", code, inv.output())
		}
		if after := c.ids(spool.QueueDir); !slices.Equal(after, before) {
			t.Errorf("queue/ = %v, want %v untouched", after, before)
		}
	})
	t.Run("reserve-left-for-root", func(t *testing.T) {
		c := newSpoolCase(t)
		c.config = limits + twoTargets
		for uid := 1000; uid < 1004; uid++ {
			c.service.reply("a", delivery.Temp, delivery.Temp)
			c.send("one", elevated(uid))
			c.send("two", elevated(uid))
		}
		c.service.reply("a", delivery.Temp, delivery.Temp)
		if code, _ := c.send("full", elevated(1004)); code != 73 {
			t.Fatalf("fifth user Run() = %d, want 73 with 80%% of the spool taken", code)
		}
		if code, inv := c.send("root", rootCaller); code != 0 || len(c.ids(spool.QueueDir)) != 9 {
			t.Fatalf("root Run() = %d, queue %d entries; output:\n%s", code, len(c.ids(spool.QueueDir)), inv.output())
		}
	})
}

// TestHold covers messages kept while the configuration is rejected.
func TestHold(t *testing.T) {
	t.Run("T-ADJ-28/invalid-configuration-holds-the-message", func(t *testing.T) {
		c := newSpoolCase(t)
		c.config = "[target.a]\ntype = \"http\"\n"
		code, inv := c.send("held", elevatedUser)
		if code != 78 || !strings.Contains(inv.output(), `level=WARN msg="message held" id=`) {
			t.Fatalf("Run() = %d, want 78; output:\n%s", code, inv.output())
		}
		e := c.entry(spool.HoldDir)
		if e.OwnerUID != 1000 || len(e.Targets) != 0 || e.Reason != reasonConfig {
			t.Errorf("held entry = %+v", e)
		}
		if code, _ := c.queueRun(serviceCaller); code != 78 || len(c.ids(spool.HoldDir)) != 1 {
			t.Fatalf("-q with the configuration still broken = %d, hold %v", code, c.ids(spool.HoldDir))
		}
		c.config = twoTargets
		c.clock.advance(time.Minute)
		if code, inv := c.queueRun(serviceCaller); code != 0 {
			t.Fatalf("-q = %d; output:\n%s", code, inv.output())
		}
		if len(c.ids(spool.HoldDir)) != 0 || len(c.ids(spool.QueueDir)) != 0 ||
			!slices.Equal(c.service.got("a"), []string{"held"}) || !slices.Equal(c.service.got("b"), []string{"held"}) {
			t.Errorf("hold %v, queue %v, a %v, b %v", c.ids(spool.HoldDir), c.ids(spool.QueueDir), c.service.got("a"), c.service.got("b"))
		}
	})
	t.Run("released-message-queued-on-failure", func(t *testing.T) {
		c := newSpoolCase(t)
		c.config = "[target.a]\ntype = \"http\"\n"
		c.send("held", elevatedUser)
		c.config = twoTargets
		c.service.reply("b", delivery.Temp)
		c.queueRun(serviceCaller)
		if e := c.entry(spool.QueueDir); e.Targets["a"].State != spool.Done || e.Targets["b"].State != spool.Pending || e.OwnerUID != 1000 {
			t.Errorf("released entry = %+v", e)
		}
	})
	t.Run("hold-expires", func(t *testing.T) {
		c := newSpoolCase(t)
		c.config = "[target.a]\ntype = \"http\"\n"
		c.send("held", elevatedUser)
		c.clock.advance(7*24*time.Hour + time.Second)
		c.queueRun(serviceCaller)
		if len(c.ids(spool.HoldDir)) != 0 || len(c.ids(spool.FailedDir)) != 1 {
			t.Errorf("hold %v, failed %v, want the entry failed", c.ids(spool.HoldDir), c.ids(spool.FailedDir))
		}
	})
	t.Run("not-held-exits-73", func(t *testing.T) {
		for _, config := range []string{"[target.a]\ntype = \"http\"\n", routeRootToA} {
			c := newSpoolCase(t)
			c.dir = filepath.Join(c.dir, "missing")
			c.config = config
			code, inv := c.sendInput("To: alice\nSubject: x\n\nb\n")
			if code != 73 || !strings.Contains(inv.output(), `level=ERROR msg="message lost, not held"`) {
				t.Errorf("Run() = %d, want 73 for a message not held; output:\n%s", code, inv.output())
			}
		}
	})
	t.Run("not-written-exits-74", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root ignores directory modes")
		}
		c := newSpoolCase(t)
		if _, err := spool.Open(c.dir); err != nil {
			t.Fatal(err)
		}
		holdDir := filepath.Join(c.dir, spool.HoldDir)
		if err := os.Chmod(holdDir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(holdDir, 0o770) })
		c.config = "[target.a]\ntype = \"http\"\n"
		if code, inv := c.send("lost", elevatedUser); code != 74 || len(c.ids(spool.TmpDir)) != 0 {
			t.Errorf("Run() = %d, tmp %v, want 74 and nothing left; output:\n%s", code, c.ids(spool.TmpDir), inv.output())
		}
	})
	t.Run("rejected-target-holds", func(t *testing.T) {
		c := newSpoolCase(t)
		c.config = "[target.x]\ntype = \"shoutrrr\"\nurl = \"nosuch://example.org\"\n"
		if code, _ := c.send("held", elevatedUser); code != 78 || len(c.ids(spool.HoldDir)) != 1 {
			t.Errorf("Run() = %d, hold %v", code, c.ids(spool.HoldDir))
		}
	})
}

// TestListQueue covers mailq, -bp and --status.
func TestListQueue(t *testing.T) {
	c := newSpoolCase(t)
	c.service.reply("a", delivery.Temp)
	c.send("queued", elevatedUser)
	c.config = "[target.a]\ntype = \"http\"\n"
	c.send("held", elevated(1001))
	c.config = twoTargets
	c.clock.advance(90 * time.Second)
	ids := append(c.ids(spool.QueueDir), c.ids(spool.HoldDir)...)
	t.Run("T-MTA-31/bp-lists-entries-for-the-service-user", func(t *testing.T) {
		code, inv := c.queueRun(serviceCaller, "-bp")
		want := "1 queued, 1 held, 0 failed; oldest 1m30s\n" +
			ids[0] + " queue uid=1000 age=1m30s\n" +
			"  a pending attempts=1 next=2026-09-27T10:01:00Z temp=\"temporary failure, status 503: unavailable\"\n" +
			"  b done attempts=0\n" +
			ids[1] + " hold uid=1001 age=1m30s reason=\"configuration rejected\"\n"
		if code != 0 || inv.stdout.String() != want {
			t.Errorf("-bp = %d, stdout:\n%s\nwant:\n%s", code, inv.stdout.String(), want)
		}
	})
	t.Run("T-MTA-31/mailq-counts-only-for-a-user", func(t *testing.T) {
		inv := c.invocation(elevatedUser, nil)
		inv.program = "/usr/bin/mailq"
		if code := inv.run(t); code != 0 || inv.stdout.String() != "1 queued, 1 held, 0 failed; oldest 1m30s\n" {
			t.Errorf("mailq = %d, stdout:\n%s", code, inv.stdout.String())
		}
	})
	t.Run("status", func(t *testing.T) {
		code, inv := c.queueRun(elevatedUser, "--status")
		if code != 0 || !strings.HasPrefix(inv.stdout.String(), "queued=1 held=1 failed=0 tmp=0 bytes=") ||
			!strings.HasSuffix(inv.stdout.String(), " oldest_age_seconds=90\n") {
			t.Errorf("--status = %d, stdout %q", code, inv.stdout.String())
		}
	})
	t.Run("output-not-written-exits-74", func(t *testing.T) {
		for _, args := range [][]string{{"-bp"}, {"--status"}} {
			inv := c.invocation(serviceCaller, args)
			inv.stdoutWriter = failingWriter{}
			if code := inv.run(t); code != 74 || !strings.Contains(inv.output(), `level=ERROR msg="queue listing not written" err="no space left on device"`) {
				t.Errorf("%v = %d, want 74; output:\n%s", args, code, inv.output())
			}
		}
	})
	t.Run("T-MTA-31/empty-spool", func(t *testing.T) {
		empty := newSpoolCase(t)
		if code, inv := empty.queueRun(serviceCaller, "-bp"); code != 0 || inv.stdout.String() != "queue is empty\n" {
			t.Errorf("-bp = %d, stdout %q", code, inv.stdout.String())
		}
	})
}

// failingWriter fails every write as a full disk does.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, syscall.ENOSPC }

// TestQueueRemoveFailureSaves pins the fallback of the last result: when
// the removal of the finished entry fails, the result is saved in a new
// sidecar, and the removal that finish tries again is logged and makes -q
// exit 74. A non-empty directory in place of the message file lets the
// sidecar go and fails the unlink of the message.
func TestQueueRemoveFailureSaves(t *testing.T) {
	c := newSpoolCase(t)
	c.service.reply("a", delivery.Temp)
	if code, _ := c.send("s", elevatedUser); code != 0 {
		t.Fatalf("Run() = %d", code)
	}
	id := c.ids(spool.QueueDir)[0]
	c.clock.advance(time.Hour)
	inv := c.invocation(serviceCaller, []string{"-q"})
	inv.deliver = func(ctx context.Context, targets []delivery.Target, env message.Envelope, d render.Data, files []message.Attachment) []delivery.Result {
		eml := filepath.Join(c.dir, spool.QueueDir, id+".eml")
		if err := os.Remove(eml); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(eml, "keep"), 0o700); err != nil {
			t.Fatal(err)
		}
		return c.service.deliver(ctx, targets, env, d, files)
	}
	// The sidecar is read in the hook: finish removes it again afterwards.
	var saved []*spool.Entry
	inv.spoolSaved = func(e *spool.Entry) {
		if _, err := os.Stat(filepath.Join(c.dir, spool.QueueDir, id+".json")); err != nil {
			t.Errorf("sidecar after the failed removal: %v", err)
		}
		saved = append(saved, e)
	}
	code := inv.run(t)
	if code != 74 || !strings.Contains(inv.output(), `level=ERROR msg="spool entry not removed" id=`+id) ||
		strings.Contains(inv.output(), "spool entry not updated") {
		t.Errorf("-q = %d, want 74 and the removal logged; output:\n%s", code, inv.output())
	}
	if len(saved) != 1 || saved[0].IsPending() {
		t.Errorf("saved %d sidecars, want one with the last result recorded", len(saved))
	}
}

// corrupt overwrites the sidecar of the only entry in area with text that
// does not decode and dates its message file at written; it returns the id.
func (c *spoolCase) corrupt(area string, written time.Time) string {
	c.t.Helper()
	id := c.ids(area)[0]
	path := filepath.Join(c.dir, area, id)
	if err := os.WriteFile(path+".json", []byte("{"), 0o660); err != nil {
		c.t.Fatal(err)
	}
	if err := os.Chtimes(path+".eml", written, written); err != nil {
		c.t.Fatal(err)
	}
	return id
}

// TestQueueCorruptSidecar pins what a queue run does with an entry whose
// sidecar does not decode: it warns and exits 74 until the message file is
// older than the TTL of its area, then moves the entry to failed/, whose
// failed_ttl deletes it.
func TestQueueCorruptSidecar(t *testing.T) {
	t.Run("queue-entry-fails-after-queue-ttl", func(t *testing.T) {
		c := newSpoolCase(t)
		c.service.reply("a", delivery.Temp)
		c.send("s", elevatedUser)
		id := c.corrupt(spool.QueueDir, testNow.Add(-time.Hour))
		code, inv := c.queueRun(serviceCaller)
		if code != 74 || !strings.Contains(inv.output(), `level=WARN msg="spool entry unreadable, left alone" id=`+id+` area=queue err="corrupt sidecar: `) {
			t.Fatalf("-q = %d, want 74 and a warning; output:\n%s", code, inv.output())
		}
		c.corrupt(spool.QueueDir, testNow.Add(-7*24*time.Hour-time.Minute))
		code, inv = c.queueRun(serviceCaller)
		if code != 0 || !strings.Contains(inv.output(), `level=ERROR msg="message failed" id=`+id+` reason="corrupt sidecar"`) {
			t.Fatalf("-q = %d, want 0 and the entry failed; output:\n%s", code, inv.output())
		}
		if e := c.entry(spool.FailedDir); e.ID != id || e.OwnerUID != os.Getuid() || e.Reason != reasonCorrupt || !e.FailedAt.Equal(c.clock.now()) {
			t.Errorf("failed entry = %+v", e)
		}
		if _, inv := c.queueRun(serviceCaller, "-bp"); !strings.Contains(inv.stdout.String(), id+` failed uid=`) {
			t.Errorf("-bp does not list the entry:\n%s", inv.stdout.String())
		}
		c.clock.advance(30*24*time.Hour + time.Minute)
		if code, _ := c.queueRun(serviceCaller); code != 0 || len(c.ids(spool.FailedDir)) != 0 {
			t.Errorf("-q = %d, failed %v, want the entry deleted after failed_ttl", code, c.ids(spool.FailedDir))
		}
	})
	t.Run("held-entry-fails-after-hold-ttl", func(t *testing.T) {
		c := newSpoolCase(t)
		c.config = "[target.a]\ntype = \"http\"\n"
		c.send("held", elevatedUser)
		id := c.corrupt(spool.HoldDir, testNow.Add(-2*time.Hour))
		c.config = "[spool]\nhold_ttl = \"1h\"\n\n" + twoTargets
		if code, inv := c.queueRun(serviceCaller); code != 0 || len(c.ids(spool.FailedDir)) != 1 {
			t.Fatalf("-q = %d, failed %v, want %s failed after hold_ttl; output:\n%s", code, c.ids(spool.FailedDir), id, inv.output())
		}
	})
	t.Run("failed-entry-deleted-after-failed-ttl", func(t *testing.T) {
		c := newSpoolCase(t)
		c.config = "[target.a]\ntype = \"http\"\n"
		c.send("held", elevatedUser)
		c.clock.advance(7*24*time.Hour + time.Second)
		c.queueRun(serviceCaller)
		c.corrupt(spool.FailedDir, c.clock.now().Add(-29*24*time.Hour))
		if c.queueRun(serviceCaller); len(c.ids(spool.FailedDir)) != 1 {
			t.Fatalf("failed %v, want the entry kept within failed_ttl", c.ids(spool.FailedDir))
		}
		c.corrupt(spool.FailedDir, c.clock.now().Add(-31*24*time.Hour))
		if c.queueRun(serviceCaller); len(c.ids(spool.FailedDir)) != 0 {
			t.Errorf("failed %v, want the entry deleted", c.ids(spool.FailedDir))
		}
	})
}

// TestQueueErrorRedacted pins that the error stored in the sidecar and
// shown by mailq passes through the redactor.
func TestQueueErrorRedacted(t *testing.T) {
	const token = "123456:SECRET-TOKEN-VALUE"
	c := newSpoolCase(t)
	c.config = "[target.tg]\ntype = \"telegram\"\ntoken = \"" + token + "\"\nchat_id = 1\n"
	c.service = newService()
	inv := c.invocation(elevatedUser, nil)
	inv.stdin = strings.NewReader("Subject: s\n\nb\n")
	inv.deliver = func(_ context.Context, targets []delivery.Target, _ message.Envelope, _ render.Data, _ []message.Attachment) []delivery.Result {
		return []delivery.Result{{TargetID: targets[0].ID, Status: delivery.Temp, Err: &backend.Error{Class: backend.Temporary, Err: fmt.Errorf("dial bot%s: refused", token)}}}
	}
	if code := inv.run(t); code != 0 {
		t.Fatalf("Run() = %d; output:\n%s", code, inv.output())
	}
	data, err := os.ReadFile(filepath.Join(c.dir, spool.QueueDir, c.ids(spool.QueueDir)[0]+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "SECRET") || !strings.Contains(string(data), "***") {
		t.Errorf("sidecar keeps the token:\n%s", data)
	}
}

// TestHoldInDefaultSpool covers a message held in the default directory
// while the file, which names a directory of its own, was rejected.
func TestHoldInDefaultSpool(t *testing.T) {
	c := newSpoolCase(t)
	own := t.TempDir()
	c.config = "[spool]\ndir = \"" + own + "\"\n\n[target.a]\ntype = \"http\"\n"
	if code, _ := c.send("held", elevatedUser); code != 78 || len(c.ids(spool.HoldDir)) != 1 {
		t.Fatalf("Run() = %d, default hold %v, want 78 and the message held there", code, c.ids(spool.HoldDir))
	}
	c.config = "[spool]\ndir = \"" + own + "\"\n\n" + twoTargets
	c.service.reply("b", delivery.Temp)
	code, inv := c.queueRun(serviceCaller)
	if code != 0 || !strings.Contains(inv.output(), `msg="held message released" id=`) {
		t.Fatalf("-q = %d; output:\n%s", code, inv.output())
	}
	if !slices.Equal(c.service.got("a"), []string{"held"}) || len(c.ids(spool.HoldDir)) != 0 {
		t.Errorf("a got %v, default hold %v, want delivered and released", c.service.got("a"), c.ids(spool.HoldDir))
	}
	sp, err := spool.Open(own)
	if err != nil {
		t.Fatal(err)
	}
	ids, _ := sp.List(spool.QueueDir)
	if len(ids) != 1 {
		t.Fatalf("own queue/ = %v, want the entry waiting for b", ids)
	}
	if e, _ := sp.Peek(spool.QueueDir, ids[0]); e.OwnerUID != 1000 || e.Targets["b"].State != spool.Pending || e.ReleasedAt.IsZero() {
		t.Errorf("released entry = %+v", e)
	}
}

// TestListHoldOfDefaultSpool pins that mailq and --status show where a
// held message is: the hold/ of the default directory beside a dir of the
// file, and the dir of a file whose targets are rejected.
func TestListHoldOfDefaultSpool(t *testing.T) {
	t.Run("default-hold-beside-own-dir", func(t *testing.T) {
		c := newSpoolCase(t)
		own := t.TempDir()
		c.config = "[spool]\ndir = \"" + own + "\"\n\n[target.a]\ntype = \"http\"\n"
		c.send("held", elevatedUser)
		id := c.ids(spool.HoldDir)[0]
		c.config = "[spool]\ndir = \"" + own + "\"\n\n" + twoTargets
		want := "0 queued, 1 held, 0 failed; oldest 0s\n" +
			id + ` hold uid=1000 age=0s reason="configuration rejected" dir="` + c.dir + "\"\n"
		if code, inv := c.queueRun(serviceCaller, "-bp"); code != 0 || inv.stdout.String() != want {
			t.Errorf("-bp = %d, stdout:\n%s\nwant:\n%s", code, inv.stdout.String(), want)
		}
		if code, inv := c.queueRun(serviceCaller, "--status"); code != 0 || !strings.HasPrefix(inv.stdout.String(), "queued=0 held=1 ") {
			t.Errorf("--status = %d, stdout %q, want the held message counted", code, inv.stdout.String())
		}
	})
	t.Run("default-hold-unreadable-warns", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root ignores directory modes")
		}
		c := newSpoolCase(t)
		own := t.TempDir()
		if _, err := spool.Open(c.dir); err != nil {
			t.Fatal(err)
		}
		holdDir := filepath.Join(c.dir, spool.HoldDir)
		if err := os.Chmod(holdDir, 0o300); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(holdDir, 0o770) })
		c.config = "[spool]\ndir = \"" + own + "\"\n\n" + twoTargets
		code, inv := c.queueRun(serviceCaller, "--status")
		if code != 0 || !strings.HasPrefix(inv.stdout.String(), "queued=0 held=0 ") ||
			!strings.Contains(inv.output(), `level=WARN msg="default spool not listed" err=`) {
			t.Errorf("--status = %d, stdout %q, want the own counts and a warning; output:\n%s", code, inv.stdout.String(), inv.output())
		}
	})
	t.Run("rejected-targets-list-own-dir", func(t *testing.T) {
		c := newSpoolCase(t)
		own := t.TempDir()
		c.config = "[spool]\ndir = \"" + own + "\"\n\n[target.x]\ntype = \"shoutrrr\"\nurl = \"nosuch://example.org\"\n"
		c.send("held", elevatedUser)
		code, inv := c.queueRun(serviceCaller, "-bp")
		if code != 0 || !strings.HasPrefix(inv.stdout.String(), "0 queued, 1 held, 0 failed;") ||
			!strings.Contains(inv.output(), `level=WARN msg="spool listed under a rejected configuration" dir=`+own) {
			t.Errorf("-bp = %d, stdout %q, want the message held in %s; output:\n%s", code, inv.stdout.String(), own, inv.output())
		}
	})
}

// TestHoldReleaseRestartsTTL pins that queue_ttl counts from the release,
// not from the time the message was held.
func TestHoldReleaseRestartsTTL(t *testing.T) {
	c := newSpoolCase(t)
	c.config = "[target.a]\ntype = \"http\"\n"
	c.send("held", elevatedUser)
	c.clock.advance(6 * 24 * time.Hour)
	c.config = twoTargets
	c.service.reply("a", delivery.Temp)
	c.queueRun(serviceCaller)
	c.clock.advance(3 * 24 * time.Hour)
	c.service.reply("a", delivery.Temp)
	c.queueRun(serviceCaller)
	if len(c.ids(spool.FailedDir)) != 0 || len(c.ids(spool.QueueDir)) != 1 {
		t.Fatalf("failed %v, queue %v, want the entry still queued 9 days after it was held", c.ids(spool.FailedDir), c.ids(spool.QueueDir))
	}
	c.clock.advance(4*24*time.Hour + time.Second)
	c.queueRun(serviceCaller)
	if len(c.ids(spool.FailedDir)) != 1 {
		t.Errorf("failed %v, want the entry expired 7 days after its release", c.ids(spool.FailedDir))
	}
}

// TestListMissingSpool covers the listing modes against a directory that
// does not exist, or exists empty.
func TestListMissingSpool(t *testing.T) {
	c := newSpoolCase(t)
	c.dir = filepath.Join(c.dir, "missing")
	if code, inv := c.queueRun(elevatedUser, "-bp"); code != 0 || inv.stdout.String() != "queue is empty\n" {
		t.Errorf("-bp = %d, stdout %q, want an empty queue", code, inv.stdout.String())
	}
	if code, _ := c.queueRun(elevatedUser, "--status"); code != 74 {
		t.Errorf("--status = %d, want 74", code)
	}
	empty := newSpoolCase(t)
	for _, args := range [][]string{{"-bp"}, {"--status"}} {
		if code, _ := empty.queueRun(serviceCaller, args...); code != 0 {
			t.Errorf("%v = %d", args, code)
		}
	}
	if entries, _ := os.ReadDir(empty.dir); len(entries) != 0 {
		t.Errorf("listing created %v", entries)
	}
}

// TestDefaultSpoolExpiredAndStale pins that with a dir of its own, -q
// moves an expired message held in the default directory into its own
// failed/, where failed_ttl deletes it, and removes the stale files of the
// default directory.
func TestDefaultSpoolExpiredAndStale(t *testing.T) {
	c := newSpoolCase(t)
	own := t.TempDir()
	c.config = "[spool]\ndir = \"" + own + "\"\n\n[target.a]\ntype = \"http\"\n"
	c.send("held", elevatedUser)
	stale := filepath.Join(c.dir, spool.TmpDir, "1-00000000000000aa.eml")
	if err := os.WriteFile(stale, []byte("x"), 0o660); err != nil {
		t.Fatal(err)
	}
	c.config = "[spool]\ndir = \"" + own + "\"\n\n" + twoTargets
	c.clock.advance(7*24*time.Hour + time.Second)
	if err := os.Chtimes(stale, c.clock.now().Add(-2*time.Hour), c.clock.now().Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	code, inv := c.queueRun(serviceCaller)
	if code != 0 || len(c.service.got("a")) != 0 {
		t.Fatalf("-q = %d, a got %v; output:\n%s", code, c.service.got("a"), inv.output())
	}
	sp, err := spool.Open(own)
	if err != nil {
		t.Fatal(err)
	}
	failed, _ := sp.List(spool.FailedDir)
	if len(failed) != 1 || len(c.ids(spool.HoldDir)) != 0 || len(c.ids(spool.TmpDir)) != 0 {
		t.Fatalf("own failed/ %v, default hold/ %v, default tmp/ %v, want the entry failed here and the default spool clean",
			failed, c.ids(spool.HoldDir), c.ids(spool.TmpDir))
	}
	c.clock.advance(30*24*time.Hour + time.Second)
	c.queueRun(serviceCaller)
	if failed, _ := sp.List(spool.FailedDir); len(failed) != 0 {
		t.Errorf("own failed/ = %v after failed_ttl", failed)
	}
}

// TestReleaseKeepsExistingCopy pins that a held entry whose copy a run
// killed before the removal already put into the queue does not replace
// that copy, which another process may be delivering.
func TestReleaseKeepsExistingCopy(t *testing.T) {
	c := newSpoolCase(t)
	own := t.TempDir()
	c.config = "[spool]\ndir = \"" + own + "\"\n\n[target.a]\ntype = \"http\"\n"
	c.send("held", elevatedUser)
	def, err := spool.Open(c.dir)
	if err != nil {
		t.Fatal(err)
	}
	id := c.ids(spool.HoldDir)[0]
	rec, err := def.Lock(spool.HoldDir, id)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := rec.Message()
	entry := *rec.Entry
	_ = rec.Close()
	sp, err := spool.Open(own)
	if err != nil {
		t.Fatal(err)
	}
	entry.SetTargets([]string{"a", "b"})
	entry.MarkDone("a")
	copied, err := sp.Create(spool.QueueDir, &entry, raw, spool.Quota{})
	if err != nil {
		t.Fatal(err)
	}
	// The copy stays locked, as by a process delivering it.
	defer func() { _ = copied.Close() }()
	before, err := os.Stat(filepath.Join(own, spool.QueueDir, id+".eml"))
	if err != nil {
		t.Fatal(err)
	}
	c.config = "[spool]\ndir = \"" + own + "\"\n\n" + twoTargets
	code, inv := c.queueRun(serviceCaller)
	if code != 0 || !strings.Contains(inv.output(), `msg="held message already released" id=`+id) {
		t.Fatalf("-q = %d; output:\n%s", code, inv.output())
	}
	after, err := os.Stat(filepath.Join(own, spool.QueueDir, id+".eml"))
	if err != nil || !os.SameFile(before, after) {
		t.Errorf("the queued copy was replaced: %v", err)
	}
	if len(c.ids(spool.HoldDir)) != 0 || len(c.service.got("a"))+len(c.service.got("b")) != 0 {
		t.Errorf("default hold/ %v, a %v, b %v, want the held entry removed and the locked copy left to its holder",
			c.ids(spool.HoldDir), c.service.got("a"), c.service.got("b"))
	}
}

// TestLoadTargetsKeepsConfigTag pins that a queue run whose configuration
// loads but whose targets fail logs the rejection under the syslog tag of
// that configuration.
func TestLoadTargetsKeepsConfigTag(t *testing.T) {
	inv := &invocation{
		config: "[general]\nsyslog_tag = \"custom\"\n\n[target.dc]\ntype = \"discord\"\nurl = \"https://example.org/x\"\ntemplate = \"{{ .Nope\"\n",
		args:   []string{"-q"},
		stdin:  iotest.ErrReader(errors.New("stdin read")),
	}
	if code := inv.run(t); code != 78 {
		t.Fatalf("Run() = %d, want 78; output:\n%s", code, inv.output())
	}
	if log := inv.log("custom"); !strings.Contains(log, `level=ERROR msg="configuration rejected"`) {
		t.Errorf("log of tag custom lacks the rejection:\n%s", inv.output())
	}
}

// BenchmarkOwnMessage measures one call that spools its message and
// delivers it to every target at once, the fsyncs of the spool included:
// the spool is in the temporary directory, so a TMPDIR on tmpfs syncs
// nothing. go test -run '^$' -bench OwnMessage ./internal/app
func BenchmarkOwnMessage(b *testing.B) {
	deliver := func(_ context.Context, targets []delivery.Target, _ message.Envelope, _ render.Data, _ []message.Attachment) []delivery.Result {
		results := make([]delivery.Result, len(targets))
		for i, target := range targets {
			results[i] = delivery.Result{TargetID: target.ID, Status: delivery.OK}
		}
		return results
	}
	for _, n := range []int{1, 3} {
		b.Run(fmt.Sprintf("targets=%d", n), func(b *testing.B) {
			var config strings.Builder
			for i := range n {
				fmt.Fprintf(&config, "[target.t%d]\ntype = \"http\"\npreset = \"generic-json\"\nurl = \"http://127.0.0.1:1/\"\n\n", i)
			}
			dir := b.TempDir()
			for b.Loop() {
				inv := &invocation{config: config.String(), spoolDir: dir, stdin: strings.NewReader("Subject: disk full\n\n/dev/sda1 99%\n"), deliver: deliver}
				if code := inv.run(b); code != 0 {
					b.Fatalf("Run() = %d; output:\n%s", code, inv.output())
				}
			}
			if ids, _ := os.ReadDir(filepath.Join(dir, spool.QueueDir)); len(ids) != 0 {
				b.Fatalf("queue/ keeps %d files", len(ids))
			}
		})
	}
}
