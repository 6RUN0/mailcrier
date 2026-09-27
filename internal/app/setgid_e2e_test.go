//go:build setgid_e2e

package app

import (
	"bytes"
	"context"
	"encoding/pem"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// slendmailBinary is the binary under test, built with CGO_ENABLED=0.
var slendmailBinary = flag.String("slendmail-binary", "", "path of the slendmail binary to install setgid")

// Ids of the throwaway system user and group; nobody runs the binary.
const (
	serviceID  = 990
	nobodyUser = "nobody"
)

// successfulExec matches a completed execve line of strace -f output.
var successfulExec = regexp.MustCompile(`(?m)^\d+ +execve\("([^"]+)".*= 0$`)

// TestSetgidReexec installs the binary setgid in a throwaway root file
// system (the setgid-e2e make target runs it in a container) and runs it
// as an unprivileged user with an environment an attacker would set:
// GODEBUG=http2debug=2 to print request paths and frames, proxy variables
// pointing at a listener, TLS variables pointing at no roots, and a
// configuration override. The process must execute itself exactly once,
// keep its group (the configuration is readable only through it), deliver
// over HTTP/2 without printing the token or the message, and ignore the
// proxy, the TLS variables and the override.
func TestSetgidReexec(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatal("must run as root: it installs a setgid binary and a system group")
	}
	if *slendmailBinary == "" {
		t.Fatal("-slendmail-binary is required")
	}
	strace, err := exec.LookPath("strace")
	if err != nil {
		t.Fatal("strace is required to count execve calls:", err)
	}
	// Readable by nobody, so that honouring the override or the bundle
	// would show; t.TempDir is private to root.
	dir, err := os.MkdirTemp("", "setgid-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	receiver := httptest.NewUnstartedServer(nil)
	type request struct{ proto, path string }
	requests := make(chan request, 4)
	receiver.Config.Handler = http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		requests <- request{r.Proto, r.URL.Path}
	})
	receiver.EnableHTTP2 = true
	receiver.StartTLS()
	defer receiver.Close()
	evil := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the overriding configuration was used")
	}))
	defer evil.Close()
	proxy, proxyConns := listenCounting(t)

	appendFile(t, "/etc/group", fmt.Sprintf("slendmail:x:%d:\n", serviceID))
	appendFile(t, "/etc/passwd", fmt.Sprintf("slendmail:x:%d:%d::/nonexistent:/usr/sbin/nologin\n", serviceID, serviceID))
	installFile(t, *slendmailBinary, "/usr/sbin/slendmail", 0, serviceID, 0o755|os.ModeSetgid)
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: receiver.Certificate().Raw})
	writeFile(t, "/etc/ssl/certs/ca-certificates.crt", ca, 0, 0, 0o644)
	writeFile(t, "/etc/slendmail.conf", []byte(httpTargetConfig(receiver.URL+"/hook/"+secretToken)), 0, serviceID, 0o640)
	evilConfig := filepath.Join(dir, "evil.conf")
	writeFile(t, evilConfig, []byte(httpTargetConfig(evil.URL)), 0, 0, 0o644)
	// Both variables, because with an empty SSL_CERT_FILE alone Go still
	// loads the system directory.
	emptyBundle := filepath.Join(dir, "empty.pem")
	writeFile(t, emptyBundle, nil, 0, 0, 0o644)
	emptyCertDir := filepath.Join(dir, "certs")
	if err := os.Mkdir(emptyCertDir, 0o755); err != nil {
		t.Fatal(err)
	}

	trace := filepath.Join(dir, "trace")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, strace, "-f", "-qq", "-e", "trace=execve,umask,openat", "-o", trace, "-u", nobodyUser,
		"/usr/sbin/slendmail", "--config", evilConfig, "-ti")
	cmd.Env = []string{
		"PATH=/usr/sbin:/usr/bin:/sbin:/bin",
		"USER=" + nobodyUser,
		"GODEBUG=http2debug=2",
		"HTTPS_PROXY=http://" + proxy,
		"HTTP_PROXY=http://" + proxy,
		"SSL_CERT_FILE=" + emptyBundle,
		"SSL_CERT_DIR=" + emptyCertDir,
		"SLENDMAIL_CONFIG=" + evilConfig,
		// The time package opens a TZ path as a zone file; before the
		// re-exec that would happen with the group of the binary.
		"TZ=/etc/slendmail.conf",
	}
	const bodyMarker = "BODY-MARKER-5c1d"
	cmd.Stdin = strings.NewReader("Subject: setgid e2e\n\n" + bodyMarker + "\n")
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	runErr := cmd.Run()
	t.Logf("output:\n%s", output.String())
	if runErr != nil {
		t.Errorf("slendmail failed: %v", runErr)
	}

	select {
	case got := <-requests:
		if got.proto != "HTTP/2.0" || got.path != "/hook/"+secretToken {
			t.Errorf("receiver got %s %s, want HTTP/2.0 /hook/<token>", got.proto, got.path)
		}
	default:
		t.Error("receiver got no request")
	}
	for _, leak := range []string{secretToken, bodyMarker, "http2:"} {
		if strings.Contains(output.String(), leak) {
			t.Errorf("output contains %q", leak)
		}
	}
	if n := proxyConns.Load(); n != 0 {
		t.Errorf("proxy got %d connections, want 0", n)
	}
	// Without syslog an elevated process tells its caller only constant
	// messages: one warning per ignored override.
	wantOutput := "slendmail: syslog unavailable, logging to stderr\n" +
		"slendmail: configuration override ignored\n" +
		"slendmail: configuration override ignored\n"
	if output.String() != wantOutput {
		t.Errorf("output =\n%s\nwant\n%s", output.String(), wantOutput)
	}

	traced, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("strace:\n%s", traced)
	execs := successfulExec.FindAllStringSubmatch(string(traced), -1)
	if len(execs) != 2 || execs[0][1] != "/usr/sbin/slendmail" || execs[1][1] != selfExe {
		t.Errorf("successful execve calls %q, want the start and exactly one re-exec through %s", execs, selfExe)
	}
	if !strings.Contains(string(traced), "umask(007)") {
		t.Error("umask(007) not called")
	}
	lastExec := successfulExec.FindAllStringIndex(string(traced), -1)
	if len(lastExec) > 0 {
		beforeReexec := string(traced)[:lastExec[len(lastExec)-1][0]]
		if strings.Contains(beforeReexec, `openat(AT_FDCWD, "/etc/slendmail.conf"`) {
			t.Error("the configuration path from TZ was opened before the re-exec")
		}
	}
}

// listenCounting accepts TCP connections on a loopback port, counts and
// closes them.
func listenCounting(t *testing.T) (string, *atomic.Int64) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	var conns atomic.Int64
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			conns.Add(1)
			_ = conn.Close()
		}
	}()
	return listener.Addr().String(), &conns
}

func appendFile(t *testing.T, path, line string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	if _, err := file.WriteString(line); err != nil {
		t.Fatal(err)
	}
}

func installFile(t *testing.T, src, dst string, uid, gid int, mode os.FileMode) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, dst, data, uid, gid, mode)
}

// writeFile creates path with owner and mode; chmod follows chown, which
// clears the setgid bit.
func writeFile(t *testing.T, path string, data []byte, uid, gid int, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(path, uid, gid); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}
