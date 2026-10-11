package app

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/6RUN0/mailcrier/internal/delivery"
	"github.com/6RUN0/mailcrier/internal/spool"
)

// testSyslogTimeout stands in for syslogWriteTimeout, so that a test of a
// stuck daemon takes a fraction of a second.
const testSyslogTimeout = 50 * time.Millisecond

// stuckDatagramSocket returns the path of a datagram socket whose queue is
// full and which nobody reads, as /dev/log of a daemon that stopped
// reading: a sender without a bound blocks there forever.
func stuckDatagramSocket(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "log")
	stuckDatagramSocketAt(t, path)
	return path
}

// dgramQueueEnv names a value for net.unix.max_dgram_qlen that
// stuckDatagramSocketAt sets first, in the network namespace of
// TestStuckSyslogLongQueue; the kernel reads it when a socket is made.
const dgramQueueEnv = "MAILCRIER_TEST_DGRAM_QLEN"

// stuckDatagramSocketAt makes the stuck socket of stuckDatagramSocket at
// path.
func stuckDatagramSocketAt(t *testing.T, path string) {
	t.Helper()
	if qlen := os.Getenv(dgramQueueEnv); qlen != "" {
		if err := os.WriteFile("/proc/sys/net/unix/max_dgram_qlen", []byte(qlen), 0); err != nil {
			t.Fatal(err)
		}
	}
	receiver, err := net.ListenPacket("unixgram", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = receiver.Close() })
	// A sender also stops when its own send buffer is full, which comes
	// first when max_dgram_qlen is large (512 under systemd): the queue of
	// the receiver is full only once a fresh sender cannot write at all.
	for senders := 0; senders < 64; senders++ {
		filler, err := net.Dial("unixgram", path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = filler.Close() })
		written := 0
		for ; ; written++ {
			// A short deadline makes the write that finds no room fail
			// instead of blocking; one already passed would fail before
			// trying.
			if err := filler.SetWriteDeadline(time.Now().Add(testSyslogTimeout)); err != nil {
				t.Fatal(err)
			}
			if _, err := filler.Write([]byte("filler")); err != nil {
				if !isTimeout(err) {
					t.Fatalf("filling the queue: %v", err)
				}
				break
			}
			if written > 1<<20 {
				t.Fatal("the send buffer of a filler never filled")
			}
		}
		if written == 0 {
			return
		}
	}
	t.Fatal("the queue of the socket never filled")
}

// stuckSyslogTests are the tests that write to a socket of
// stuckDatagramSocket.
const stuckSyslogTests = "^(TestSyslogWriterBounded|TestRunWithStuckSyslog|TestSyslogStuckInParallel|TestSyslogRetryWithinTimeout)$"

// TestStuckSyslogLongQueue runs stuckSyslogTests again with
// net.unix.max_dgram_qlen 512, the value systemd sets and GitHub runners
// have, in a user and network namespace of their own. With a filler that
// stopped at its own full send buffer the queue was not full there, and a
// write expected to time out went through. Skipped where unprivileged user
// namespaces are refused.
func TestStuckSyslogLongQueue(t *testing.T) {
	// Serial: a forked child holds the fds of parallel tests until exec, keeping their flocks and sockets open.
	if os.Getenv(dgramQueueEnv) != "" {
		t.Skip("already in the namespace")
	}
	cmd := exec.Command(os.Args[0], "-test.run="+stuckSyslogTests, "-test.count=1", "-test.v")
	cmd.Env = append(os.Environ(), dgramQueueEnv+"=512")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:  syscall.CLONE_NEWUSER | syscall.CLONE_NEWNET,
		UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
	}
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		t.Skipf("no user and network namespace: %v", err)
	}
	if bytes.Contains(out, []byte("max_dgram_qlen: permission denied")) {
		t.Skipf("max_dgram_qlen is not writable in the namespace:\n%s", out)
	}
	if err != nil {
		t.Fatalf("%v with max_dgram_qlen 512:\n%s", err, out)
	}
	if passed := bytes.Count(out, []byte("\n--- PASS: Test")); passed != 4 {
		t.Fatalf("%d of 4 tests passed with max_dgram_qlen 512:\n%s", passed, out)
	}
}

func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// TestSyslogWriterBounded pins that a write to a syslog socket nobody
// reads gives up after the timeout: a datagram socket with a full queue,
// and a stream socket whose buffer is full, the connection never accepted.
func TestSyslogWriterBounded(t *testing.T) {
	t.Parallel()
	t.Run("datagram-queue-full", func(t *testing.T) {
		writer, err := dialSyslogAt([]string{stuckDatagramSocket(t)}, "mailcrier", testSyslogTimeout)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = writer.conn.Close() }()
		if err := writer.write(severityInfo, `msg="message received"`); !isTimeout(err) {
			t.Errorf("write() = %v, want a timeout", err)
		}
	})
	t.Run("stream-buffer-full", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "log")
		listener, err := net.Listen("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = listener.Close() }()
		writer, err := dialSyslogAt([]string{path}, "mailcrier", testSyslogTimeout)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = writer.conn.Close() }()
		line := strings.Repeat("x", 64<<10)
		for i := 0; i < 1000; i++ {
			if err = writer.write(severityInfo, line); err != nil {
				break
			}
		}
		if !isTimeout(err) {
			t.Errorf("write() = %v after the buffer filled, want a timeout", err)
		}
	})
}

// TestRunClosesSyslog pins that Run closes every syslog connection it
// opened, that of the default tag and that of syslog_tag, when it returns:
// the daemon reads both to the end.
func TestRunClosesSyslog(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "log")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	inv := &invocation{
		config:       "[general]\nsyslog_tag = \"other\"\n" + httpTargetConfig("http://127.0.0.1:1"),
		args:         []string{"--check-config"},
		creds:        rootCaller,
		syslogSocket: path,
	}
	inv.run(t)
	for _, tag := range []string{"mailcrier", "other"} {
		conn, err := listener.Accept()
		if err != nil {
			t.Fatal(err)
		}
		if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(conn)
		_ = conn.Close()
		if err != nil {
			t.Errorf("connection %s not closed after Run: %v; read %q", tag, err, data)
		}
	}
}

// TestDialSyslogNamesCause pins the error of a dial that fails: a stream
// socket whose backlog is full reports the refused stream connection,
// not the EPROTOTYPE of the datagram dial before it, and a missing first
// socket gives way to one that exists.
func TestDialSyslogNamesCause(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "log")
	fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syscall.Close(fd) }()
	if err := syscall.Bind(fd, &syscall.SockaddrUnix{Name: path}); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Listen(fd, 0); err != nil {
		t.Fatal(err)
	}
	// Connections nobody accepts fill the backlog of 0 or 1.
	var refused error
	for i := 0; i < 16 && refused == nil; i++ {
		conn, err := net.DialTimeout("unix", path, testSyslogTimeout)
		if err != nil {
			refused = err
			continue
		}
		t.Cleanup(func() { _ = conn.Close() })
	}
	if refused == nil {
		t.Fatal("the backlog never filled")
	}
	_, err = dialSyslogAt([]string{filepath.Join(dir, "missing"), path}, "mailcrier", testSyslogTimeout)
	if err == nil || !strings.Contains(err.Error(), "dial unix "+path) || errors.Is(err, syscall.EPROTOTYPE) {
		t.Errorf("dialSyslogAt() = %v, want the error of the stream dial of %s", err, path)
	}
}

// TestRunWithStuckSyslog pins that a call whose syslog daemon reads
// nothing finishes: the first record waits testSyslogTimeout, then it and
// the rest go to stderr after one warning, and the message is in the
// spool, where a hang would have left it nowhere.
func TestRunWithStuckSyslog(t *testing.T) {
	t.Parallel()
	c := newSpoolCase(t)
	c.service.reply("a", delivery.Temp)
	inv := c.invocation(elevatedUser, nil)
	inv.creds = plainUser
	inv.syslogSocket = stuckDatagramSocket(t)
	inv.stdin = strings.NewReader("Subject: s\n\nbody\n")
	if code := inv.run(t); code != 0 {
		t.Fatalf("Run() = %d, want 0; stderr:\n%s", code, inv.stderr.String())
	}
	stderr := inv.stderr.String()
	if !strings.Contains(stderr, `level=WARN msg="syslog write failed, logging to stderr"`) || !strings.Contains(stderr, "i/o timeout") ||
		!strings.Contains(stderr, `msg="message received"`) || strings.Count(stderr, "syslog write failed") != 1 {
		t.Errorf("stderr:\n%s", stderr)
	}
	if ids := c.ids(spool.QueueDir); len(ids) != 1 {
		t.Errorf("queue/ = %v, want the message", ids)
	}
}

// TestSyslogStuckInParallel pins that records logged in parallel, as the
// targets of a delivery log, wait out the timeout of a stuck daemon once
// together, not once each in turn behind the lock of the writer.
func TestSyslogStuckInParallel(t *testing.T) {
	// Serial: bounds the elapsed time by 3 x testSyslogTimeout, which neighbouring tests stretch.
	writer, err := dialSyslogAt([]string{stuckDatagramSocket(t)}, "mailcrier", testSyslogTimeout)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.conn.Close() }()
	var stderr bytes.Buffer
	logger := slog.New(newSyslogHandler(writer, newStderrLog(&stderr, false)))
	const records = 100
	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < records; i++ {
		wg.Go(func() { logger.Info("hook output", "target", i) })
	}
	wg.Wait()
	// In turn the records would take records x testSyslogTimeout, 5 s; the
	// first one waits out the timeout, and every other one goes on at once.
	if elapsed := time.Since(start); elapsed > 3*testSyslogTimeout {
		t.Errorf("%d records took %v", records, elapsed)
	}
	if got := strings.Count(stderr.String(), `msg="hook output"`); got != records {
		t.Errorf("stderr has %d records, want %d:\n%s", got, records, stderr.String())
	}
}

// TestSyslogRetryWithinTimeout pins that a write refused at once, by a
// daemon that restarted, dials the new socket and writes again within the
// same timeout: when the new daemon reads nothing either, the record still
// waits one timeout, not two.
func TestSyslogRetryWithinTimeout(t *testing.T) {
	// Serial: bounds the elapsed time by 2 x testSyslogTimeout, which neighbouring tests stretch.
	path := filepath.Join(t.TempDir(), "log")
	old, err := net.ListenPacket("unixgram", path)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := dialSyslogAt([]string{path}, "mailcrier", testSyslogTimeout)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.conn.Close() }()
	// The old daemon goes; a new one binds the path and reads nothing.
	_ = old.Close()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	stuckDatagramSocketAt(t, path)
	start := time.Now()
	err = writer.write(severityInfo, `msg="message received"`)
	if elapsed := time.Since(start); !isTimeout(err) || elapsed >= 2*testSyslogTimeout {
		t.Errorf("write() = %v after %v, want a timeout within %v", err, elapsed, 2*testSyslogTimeout)
	}
}
