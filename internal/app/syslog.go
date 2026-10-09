package app

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"
)

// syslogWriteTimeout bounds the dial of the syslog socket and every write
// to it. A daemon that reads keeps the queue of /dev/log short; a write
// that finds no room for a whole second meets a daemon that stopped
// reading, and log/syslog, which has no bound, would hold the call there
// forever, past deadline and a stop signal, with the message not yet in
// the spool. The records go to stderr after the first such write, so a
// stuck daemon costs a call that second once.
const syslogWriteTimeout = time.Second

// syslogPaths are the sockets tried in order, as log/syslog does.
var syslogPaths = []string{"/dev/log", "/var/run/syslog", "/var/run/log"}

// Severities of syslog, RFC 5424, and the mail facility, because cron and
// at discard the output of the mailer they run.
const (
	severityErr     = 3
	severityWarning = 4
	severityInfo    = 6
	severityDebug   = 7
	facilityMail    = 2 << 3
)

// syslogWriter sends records to a local syslog socket in the format of
// log/syslog, "<priority>Mmm dd hh:mm:ss tag[pid]: line", each write
// bounded by timeout.
type syslogWriter struct {
	conn    net.Conn
	path    string
	tag     string
	timeout time.Duration
	mu      sync.Mutex
}

// write sends line with severity within one timeout. A write refused at
// once, by a daemon that restarted and left a new socket, dials once more
// and repeats, as log/syslog does, within what is left of the same
// timeout; a write past the timeout does not, since the daemon holds the
// socket and reads nothing. The caller gives up on any error.
func (w *syslogWriter) write(severity int, line string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	deadline := time.Now().Add(w.timeout)
	record := []byte(fmt.Sprintf("<%d>%s %s[%d]: %s\n", facilityMail|severity, time.Now().Format(time.Stamp), w.tag, os.Getpid(), strings.TrimSuffix(line, "\n")))
	err := w.send(record, deadline)
	var netErr net.Error
	if err == nil || (errors.As(err, &netErr) && netErr.Timeout()) || w.path == "" {
		return err
	}
	conn, dialErr := dialSocket(w.path, deadline)
	if dialErr != nil {
		return err
	}
	_ = w.conn.Close()
	w.conn = conn
	return w.send(record, deadline)
}

// send writes record before deadline.
func (w *syslogWriter) send(record []byte, deadline time.Time) error {
	if err := w.conn.SetWriteDeadline(deadline); err != nil {
		return err
	}
	_, err := w.conn.Write(record)
	return err
}

// dialSyslog connects to the first syslog socket of syslogPaths that
// takes a connection, as a datagram socket or, failing that with
// EPROTOTYPE, as a stream one, the way log/syslog does.
func dialSyslog(tag string) (*syslogWriter, error) {
	return dialSyslogAt(syslogPaths, tag, syslogWriteTimeout)
}

// dialSyslogAt is dialSyslog over paths with the bound timeout. The error
// names the socket and the errno of the attempt that tells most: that of
// the stream dial on a stream socket, not the EPROTOTYPE of the datagram
// dial before it, and that of the first socket that exists.
func dialSyslogAt(paths []string, tag string, timeout time.Duration) (*syslogWriter, error) {
	var firstErr error
	for _, path := range paths {
		conn, err := dialSocket(path, time.Now().Add(timeout))
		if err == nil {
			return &syslogWriter{conn: conn, path: path, tag: tag, timeout: timeout}, nil
		}
		switch {
		case firstErr == nil:
			firstErr = err
		case errors.Is(firstErr, fs.ErrNotExist) && !errors.Is(err, fs.ErrNotExist):
			firstErr = err
		}
	}
	return nil, firstErr
}

// dialSocket connects to the socket at path before deadline, datagram
// first.
func dialSocket(path string, deadline time.Time) (net.Conn, error) {
	dialer := net.Dialer{Deadline: deadline}
	conn, err := dialer.Dial("unixgram", path)
	if errors.Is(err, syscall.EPROTOTYPE) {
		conn, err = dialer.Dial("unix", path)
	}
	return conn, err
}
