package app

import (
	"bytes"
	"errors"
	"log/slog"
	"net"
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
	receiver, err := net.ListenPacket("unixgram", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = receiver.Close() })
	filler, err := net.Dial("unixgram", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = filler.Close() })
	// A short deadline makes the write that finds no room fail instead of
	// blocking; one already passed would fail before trying.
	for i := 0; ; i++ {
		if err := filler.SetWriteDeadline(time.Now().Add(testSyslogTimeout)); err != nil {
			t.Fatal(err)
		}
		if _, err := filler.Write([]byte("filler")); err != nil {
			if !isTimeout(err) {
				t.Fatalf("filling the queue: %v", err)
			}
			return path
		}
		if i > 1<<20 {
			t.Fatal("the queue of the socket never filled")
		}
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

// TestDialSyslogNamesCause pins the error of a dial that fails: a stream
// socket whose backlog is full reports the refused stream connection,
// not the EPROTOTYPE of the datagram dial before it, and a missing first
// socket gives way to one that exists.
func TestDialSyslogNamesCause(t *testing.T) {
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
	// In turn the records would take records x testSyslogTimeout, 5 s.
	if elapsed := time.Since(start); elapsed > records*testSyslogTimeout/4 {
		t.Errorf("%d records took %v", records, elapsed)
	}
	if got := strings.Count(stderr.String(), `msg="hook output"`); got != records {
		t.Errorf("stderr has %d records, want %d:\n%s", got, records, stderr.String())
	}
}
