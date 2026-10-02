//go:build setgid_e2e

package app

import (
	"bytes"
	"context"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// slendmailBinary is the binary under test, built with CGO_ENABLED=0.
var slendmailBinary = flag.String("slendmail-binary", "", "path of the slendmail binary to install setgid")

// Ids of the throwaway system user and group; nobody runs the binary.
const (
	serviceID  = 990
	nobodyID   = 65534
	nobodyUser = "nobody"
)

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

	installSetgid(t)
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
	execs := successfulExecs(string(traced))
	if len(execs) != 2 || execs[0].path != "/usr/sbin/slendmail" || execs[1].path != selfExe {
		t.Errorf("successful execve calls %+v, want the start and exactly one re-exec through %s", execs, selfExe)
	}
	if !strings.Contains(string(traced), "umask(007)") {
		t.Error("umask(007) not called")
	}
	if len(execs) > 0 {
		beforeReexec := string(traced)[:execs[len(execs)-1].offset]
		if strings.Contains(beforeReexec, `openat(AT_FDCWD, "/etc/slendmail.conf"`) {
			t.Error("the configuration path from TZ was opened before the re-exec")
		}
	}
}

// installOnce guards installSetgid: the tests share the container.
var installOnce sync.Once

// installSetgid adds the service user and group, installs the binary
// setgid and creates the spool as the packages lay it out, once.
func installSetgid(t *testing.T) {
	t.Helper()
	installOnce.Do(func() {
		appendFile(t, "/etc/group", fmt.Sprintf("slendmail:x:%d:\n", serviceID))
		appendFile(t, "/etc/passwd", fmt.Sprintf("slendmail:x:%d:%d::/nonexistent:/usr/sbin/nologin\n", serviceID, serviceID))
		installFile(t, *slendmailBinary, "/usr/sbin/slendmail", 0, serviceID, 0o755|os.ModeSetgid)
		for _, dir := range []string{spoolDir, spoolDir + "/tmp", spoolDir + "/queue", spoolDir + "/hold", spoolDir + "/failed", spoolDir + "/locks"} {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Chown(dir, 0, serviceID); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(dir, 0o770|os.ModeSetgid); err != nil {
				t.Fatal(err)
			}
		}
	})
}

// spoolDir is the spool directory of the packages.
const spoolDir = "/var/spool/slendmail"

// TestSetgidSpool runs the spool with real ids, laid out as the packages
// do. A message of an unprivileged user sent while the configuration is
// broken is held; its files belong to the caller and the service group,
// and the caller can neither look into the spool nor see more than the
// counts in mailq. Once the configuration is fixed, -q as slendmail
// delivers the entry of the other user. A temporary failure then queues
// the next message and the call exits 0.
func TestSetgidSpool(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatal("must run as root: it installs a setgid binary and a system group")
	}
	if *slendmailBinary == "" {
		t.Fatal("-slendmail-binary is required")
	}
	var isUp atomic.Bool
	delivered := make(chan string, 4)
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isUp.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		delivered <- r.URL.Path
	}))
	defer receiver.Close()
	installSetgid(t)
	nobody := &syscall.Credential{Uid: nobodyID, Gid: nobodyID}
	service := &syscall.Credential{Uid: serviceID, Gid: serviceID}

	writeFile(t, "/etc/slendmail.conf", []byte("[target.hook]\ntype = \"http\"\n"), 0, serviceID, 0o640)
	out, err := runAs(t, nobody, "Subject: held\n\nbody\n", "/usr/sbin/slendmail", "-ti")
	if code := exitCode(err); code != 78 {
		t.Fatalf("call with a broken configuration = %d (%v), want 78\n%s", code, err, out)
	}
	checkEntryFiles(t, spoolDir+"/hold")
	if out, err := runAs(t, nobody, "", "/bin/ls", spoolDir+"/hold"); err == nil {
		t.Errorf("the caller lists the spool:\n%s", out)
	}
	if out, err := runAs(t, nobody, "", "/usr/sbin/slendmail", "-bp"); err != nil || !strings.HasPrefix(out, "0 queued, 1 held, 0 failed; oldest ") || strings.Count(out, "\n") != 1 {
		t.Errorf("mailq as the caller = %v, output:\n%s\nwant the counts line only", err, out)
	}

	writeFile(t, "/etc/slendmail.conf", []byte(httpTargetConfig(receiver.URL+"/hook")), 0, serviceID, 0o640)
	isUp.Store(true)
	if out, err := runAs(t, service, "", "/usr/sbin/slendmail", "-q"); err != nil {
		t.Fatalf("-q as slendmail failed: %v\n%s", err, out)
	}
	select {
	case path := <-delivered:
		if path != "/hook" {
			t.Errorf("receiver got %s", path)
		}
	default:
		t.Error("the queue run delivered nothing")
	}
	if out, err := runAs(t, service, "", "/usr/sbin/slendmail", "-bp"); err != nil || out != "queue is empty\n" {
		t.Errorf("mailq as slendmail = %v, output %q, want an empty queue", err, out)
	}

	isUp.Store(false)
	if out, err := runAs(t, nobody, "Subject: queued\n\nbody\n", "/usr/sbin/slendmail", "-ti"); err != nil {
		t.Fatalf("call with the receiver down = %v, want 0 for a queued message\n%s", err, out)
	}
	checkEntryFiles(t, spoolDir+"/queue")
}

// TestSetgidHookDropsGroup runs an exec target from a call of an
// unprivileged user to the setgid binary. The hook must start, not fail
// with EPERM on the change of ids, and run without the service group: it
// cannot read the configuration, and the group is not among its groups.
func TestSetgidHookDropsGroup(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatal("must run as root: it installs a setgid binary and a system group")
	}
	if *slendmailBinary == "" {
		t.Fatal("-slendmail-binary is required")
	}
	dir, err := os.MkdirTemp("", "setgid-hook-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	report := filepath.Join(dir, "report")
	hook := filepath.Join(dir, "hook")
	script := "#!/bin/sh\n{ echo \"groups $(id -G)\"; echo \"egid $(id -g)\"; cat /etc/slendmail.conf >/dev/null 2>&1 && echo \"config readable\"; } >> " + report + "\nexit 0\n"
	writeFile(t, hook, []byte(script), 0, 0, 0o755)
	installSetgid(t)
	writeFile(t, "/etc/slendmail.conf", []byte("[target.run]\ntype = \"exec\"\nargv = [\""+hook+"\"]\n"), 0, serviceID, 0o640)

	out, err := runAs(t, &syscall.Credential{Uid: nobodyID, Gid: nobodyID}, "Subject: hook\n\nbody\n", "/usr/sbin/slendmail", "-ti")
	if err != nil {
		t.Fatalf("call with an exec target = %v, want 0\n%s", err, out)
	}
	content, err := os.ReadFile(report)
	if err != nil {
		t.Fatalf("the hook did not run: %v", err)
	}
	t.Logf("hook report:\n%s", content)
	for line := range strings.Lines(string(content)) {
		key, value, _ := strings.Cut(strings.TrimSpace(line), " ")
		switch key {
		case "groups":
			if slices.Contains(strings.Fields(value), fmt.Sprint(serviceID)) {
				t.Errorf("the hook runs with the service group: groups %s", value)
			}
		case "egid":
			if value != fmt.Sprint(nobodyID) {
				t.Errorf("the hook runs with group %s, want %d", value, nobodyID)
			}
		default:
			t.Errorf("hook reported %q", line)
		}
	}
}

// checkEntryFiles checks that dir holds one entry, both files owned by
// nobody and the service group with mode 0660.
func checkEntryFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 2 {
		t.Fatalf("%s = %v, %v, want one message and its sidecar", dir, entries, err)
	}
	for _, entry := range entries {
		info, err := os.Stat(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		stat := info.Sys().(*syscall.Stat_t)
		if stat.Uid != nobodyID || stat.Gid != serviceID || info.Mode().Perm() != 0o660 {
			t.Errorf("%s: owner %d:%d mode %v, want %d:%d 0660", entry.Name(), stat.Uid, stat.Gid, info.Mode().Perm(), nobodyID, serviceID)
		}
	}
}

// exitCode returns the exit status of a finished command, -1 when it did
// not exit normally.
func exitCode(err error) int {
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exitErr):
		return exitErr.ExitCode()
	default:
		return -1
	}
}

// runAs runs a command with cred and stdin and returns stdout.
func runAs(t *testing.T, cred *syscall.Credential, stdin string, argv ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: cred}
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin"}
	cmd.Dir = "/"
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if stderr.Len() > 0 {
		t.Logf("%s stderr:\n%s", argv, stderr.String())
	}
	return stdout.String(), err
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
