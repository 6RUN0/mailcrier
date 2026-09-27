package app

import (
	"bytes"
	"errors"
	"log/slog"
	"log/syslog"
	"net"
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
	writer, err := syslog.Dial("udp", conn.LocalAddr().String(), syslog.LOG_MAIL|syslog.LOG_INFO, "slendmail")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Close() }()
	logger := slog.New(newSyslogHandler(writer)).With("msgid", "m1")

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
		if !strings.HasSuffix(strings.TrimSuffix(packet, "\n"), `slendmail[`+pidOf(packet)+`]: msg="target failed" msgid=m1 target=hook`) {
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
	start := strings.Index(packet, "slendmail[")
	end := strings.Index(packet, "]:")
	if start < 0 || end < start {
		return ""
	}
	return packet[start+len("slendmail[") : end]
}

// TestFallbackLoggerUsesStderr pins that a missing syslog daemon does not
// stop the call: the records go to stderr, after one warning.
func TestFallbackLoggerUsesStderr(t *testing.T) {
	var stderr bytes.Buffer
	dial := func(string) (*syslog.Writer, error) {
		return nil, errors.New("dial unix /dev/log: connect: no such file or directory")
	}
	logger := newFallbackLogger("slendmail", dial, &stderr, false)
	logger.Error("target failed", "target", "hook")
	lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("stderr has %d lines, want the warning and the record:\n%s", len(lines), stderr.String())
	}
	if !strings.Contains(lines[0], `level=WARN msg="syslog unavailable, logging to stderr" err="dial unix /dev/log`) {
		t.Errorf("warning line = %q", lines[0])
	}
	if !strings.Contains(lines[1], `level=ERROR msg="target failed" target=hook`) {
		t.Errorf("record line = %q", lines[1])
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
	logger := newFallbackLogger("slendmail", dial, &stderr, true).With("call", "0123")
	logger.Info("message received", "size", 10)
	logger.Error("configuration rejected, message not delivered", "err", "etc/slendmail.conf:3:1: target \"hook\"")
	logger.Error("target failed", "target", "hook", "status", 502)
	want := "slendmail: syslog unavailable, logging to stderr\nslendmail: configuration rejected, message not delivered\nslendmail: target failed\n"
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
