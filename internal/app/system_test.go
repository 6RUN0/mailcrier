package app

import (
	"bytes"
	"errors"
	"log/slog"
	"log/syslog"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// TestSyslogHandler sends records to a UDP listener standing in for the
// syslog daemon and checks the priority and the logfmt text of each.
func TestSyslogHandler(t *testing.T) {
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	writer, err := syslog.Dial("udp", conn.LocalAddr().String(), syslog.LOG_MAIL|syslog.LOG_INFO, "mailcrier")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Close() }()
	logger := slog.New(newSyslogHandler(writer, newStderrLog(&bytes.Buffer{}, false))).With("msgid", "m1")

	cases := []struct {
		log      func(msg string, args ...any)
		priority string
	}{
		{logger.Error, "<19>"},
		{logger.Warn, "<20>"},
		{logger.Info, "<22>"},
		{logger.Debug, "<23>"},
	}
	for _, tc := range cases {
		tc.log("target failed", "target", "hook")
		packet := readPacket(t, conn)
		if !strings.HasPrefix(packet, tc.priority) {
			t.Errorf("packet %q, want priority %s", packet, tc.priority)
		}
		if !strings.HasSuffix(strings.TrimSuffix(packet, "\n"), `mailcrier[`+pidOf(packet)+`]: msg="target failed" msgid=m1 target=hook`) {
			t.Errorf("packet %q lacks the logfmt record without time and level", packet)
		}
	}
}

func readPacket(t *testing.T, conn net.PacketConn) string {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 2048)
	n, _, err := conn.ReadFrom(buf)
	if err != nil {
		t.Fatal(err)
	}
	return string(buf[:n])
}

// pidOf extracts the pid that log/syslog puts after the tag.
func pidOf(packet string) string {
	start := strings.Index(packet, "mailcrier[")
	end := strings.Index(packet, "]:")
	if start < 0 || end < start {
		return ""
	}
	return packet[start+len("mailcrier[") : end]
}

// TestFallbackLoggerUsesStderr pins that a missing syslog daemon does not
// stop the call: the records go to stderr with the time in UTC, after one
// warning that carries the fields of the logger, also when a logger with
// another tag follows, which does not dial again.
func TestFallbackLoggerUsesStderr(t *testing.T) {
	var stderr bytes.Buffer
	dials := 0
	dial := func(string) (*syslog.Writer, error) {
		dials++
		return nil, errors.New("dial unixgram /dev/log: connect: no such file or directory")
	}
	fallback := newStderrLog(&stderr, false)
	newFallbackLogger("mailcrier", dial, fallback).With("call", "0123").Error("target failed", "target", "hook")
	newFallbackLogger("ops", dial, fallback).With("call", "0123").Info("probe sent")
	lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
	if len(lines) != 3 || dials != 1 {
		t.Fatalf("stderr has %d lines after %d dials, want the warning and two records after one:\n%s", len(lines), dials, stderr.String())
	}
	utc := regexp.MustCompile(`^time=\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z `)
	if !utc.MatchString(lines[0]) || !strings.Contains(lines[0], `level=WARN msg="syslog unavailable, logging to stderr" call=0123 err="dial unixgram /dev/log`) {
		t.Errorf("warning line = %q", lines[0])
	}
	if !strings.Contains(lines[1], `level=ERROR msg="target failed" call=0123 target=hook`) {
		t.Errorf("record line = %q", lines[1])
	}
}

// TestSyslogWriteFailure pins that a syslog daemon that stops taking
// records loses none: the record that failed goes to stderr after one
// warning, and so do the records after it.
func TestSyslogWriteFailure(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "log")
	conn, err := net.ListenPacket("unixgram", socket)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := syslog.Dial("unixgram", socket, syslog.LOG_MAIL|syslog.LOG_INFO, "mailcrier")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Close() }()
	var stderr bytes.Buffer
	logger := slog.New(newSyslogHandler(writer, newStderrLog(&stderr, false))).With("call", "0123")
	logger.Info("message received")
	if packet := readPacket(t, conn); !strings.Contains(packet, `msg="message received" call=0123`) {
		t.Fatalf("packet %q", packet)
	}
	// The daemon goes away: the write fails, and so does the dial that
	// log/syslog tries once more.
	_ = conn.Close()
	if err := os.Remove(socket); err != nil {
		t.Fatal(err)
	}
	logger.Error("target failed", "target", "hook")
	logger.Warn("message queued")
	lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("stderr has %d lines, want the warning and two records:\n%s", len(lines), stderr.String())
	}
	for i, want := range []string{`level=WARN msg="syslog write failed, logging to stderr" call=0123 err=`, `level=ERROR msg="target failed" call=0123 target=hook`, `level=WARN msg="message queued" call=0123`} {
		if !strings.Contains(lines[i], want) {
			t.Errorf("line %d = %q, want %q", i, lines[i], want)
		}
	}
}

// TestFallbackLoggerElevated pins that an elevated process tells its
// caller only the constant messages of warnings and errors: configuration
// positions, target names and statuses stay in syslog.
func TestFallbackLoggerElevated(t *testing.T) {
	var stderr bytes.Buffer
	dial := func(string) (*syslog.Writer, error) {
		return nil, errors.New("dial unix /dev/log: connect: no such file or directory")
	}
	logger := newFallbackLogger("mailcrier", dial, newStderrLog(&stderr, true)).With("call", "0123")
	logger.Info("message received", "size", 10)
	logger.Error("configuration rejected, message not delivered", "err", "etc/mailcrier.conf:3:1: target \"hook\"")
	logger.Error("target failed", "target", "hook", "status", 502)
	want := "mailcrier: syslog unavailable, logging to stderr\nmailcrier: configuration rejected, message not delivered\nmailcrier: target failed\n"
	if got := stderr.String(); got != want {
		t.Errorf("stderr =\n%s\nwant\n%s", got, want)
	}
}

// TestTransportProxy pins where the proxy comes from: the environment
// without elevation (a container behind a proxy), nowhere with it.
func TestTransportProxy(t *testing.T) {
	if newTransport(false).Proxy == nil {
		t.Error("unelevated transport ignores the proxy variables")
	}
	if newTransport(true).Proxy != nil {
		t.Error("elevated transport uses a proxy")
	}
}
