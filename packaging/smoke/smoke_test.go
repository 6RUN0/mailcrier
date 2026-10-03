//go:build smoke

// Package smoke installs the deb, rpm and apk packages of dist/ in
// throwaway containers of the distributions they target and drives them as
// a host would: cron and at jobs of a user, a hook, the queue run of the
// package, reinstall and removal. The smoke make targets build smoke.test
// with CGO_ENABLED=0 and run each test function as root in a fresh
// container with dist/ on /pkgs, the setgid tests of internal/app on /e2e
// and the caller fixtures on /callers; nothing here runs on the host.
package smoke

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const (
	binary    = "/usr/sbin/slendmail"
	config    = "/etc/slendmail.conf"
	spoolDir  = "/var/spool/slendmail"
	spoolNote = "slendmail: /var/spool/slendmail is left in place, it holds messages"
	// hookRecipient is the address the route of the exec target matches.
	hookRecipient = "hook@example.org"
)

// spoolAreas are the directories of the spool the packages create.
var spoolAreas = []string{"tmp", "queue", "hold", "failed", "locks"}

// mtaLinks are the paths of the sendmail interface the packages provide.
var mtaLinks = []string{"/usr/sbin/sendmail", "/usr/lib/sendmail", "/usr/bin/mailq", "/usr/bin/newaliases"}

var (
	//go:embed testdata/attachment.eml
	attachmentEML []byte
	//go:embed testdata/hook.eml
	hookEML []byte
)

// Markers of the fixtures: the subject of attachment.eml and the name of
// its attachment.
const (
	attachmentMarker = "smoke-attachment-7d41"
	attachmentName   = "report.txt"
)

// distro describes how a distribution installs, reinstalls and removes a
// package and runs cron.
type distro struct {
	name   string
	format string
	// pattern matches the amd64 package in /pkgs.
	pattern string
	install []string
	remove  []string
	// cron runs the cron daemon in the foreground.
	cron []string
	// cronFile is the queue run of the package for cron.
	cronFile string
	// mta is what /usr/sbin/sendmail resolves to before the install, empty
	// when the image has none.
	mta string
	// postfix lists the packages of postfix, which depend on each other.
	postfix []string
}

// distros are keyed by ID and the major part of VERSION_ID of
// /etc/os-release, so that a minor update of an image keeps its entry.
var distros = map[string]distro{
	"debian/13": {
		format: "deb", pattern: "/pkgs/slendmail_*_linux_amd64.deb",
		install: []string{"dpkg", "-i"}, remove: []string{"dpkg", "-r", "slendmail"},
		cron: []string{"cron", "-f", "-L", "15"}, cronFile: "/etc/cron.d/slendmail",
	},
	"rocky/9": {
		format: "rpm", pattern: "/pkgs/slendmail_*_linux_amd64.rpm",
		install: []string{"rpm", "-i"}, remove: []string{"rpm", "-e", "slendmail"},
		cron: []string{"crond", "-n", "-x", "proc"}, cronFile: "/etc/cron.d/slendmail",
		mta: "/usr/sbin/sendmail.postfix", postfix: []string{"postfix"},
	},
	"rocky/10": {
		format: "rpm", pattern: "/pkgs/slendmail_*_linux_amd64.rpm",
		install: []string{"rpm", "-i"}, remove: []string{"rpm", "-e", "slendmail"},
		cron: []string{"crond", "-n", "-x", "proc"}, cronFile: "/etc/cron.d/slendmail",
		mta: "/usr/sbin/sendmail.postfix", postfix: []string{"postfix", "postfix-lmdb"},
	},
	"alpine/3": {
		format: "apk", pattern: "/pkgs/slendmail_*_linux_amd64.apk",
		install: []string{"apk", "add", "--allow-untrusted", "--no-network"}, remove: []string{"apk", "del", "--no-network", "slendmail"},
		cron: []string{"crond", "-f", "-d", "0"}, cronFile: "/etc/crontabs/slendmail",
		mta: "/bin/busybox",
	},
}

// currentDistro returns the entry of the distribution the test runs in.
func currentDistro(t *testing.T) distro {
	t.Helper()
	data, err := os.ReadFile("/etc/os-release")
	if err != nil {
		t.Fatal(err)
	}
	fields := map[string]string{}
	for line := range strings.Lines(string(data)) {
		key, value, _ := strings.Cut(strings.TrimSpace(line), "=")
		fields[key] = strings.Trim(value, `"`)
	}
	major, _, _ := strings.Cut(fields["VERSION_ID"], ".")
	name := fields["ID"] + "/" + major
	d, ok := distros[name]
	if !ok {
		t.Fatalf("no entry for %s in the distribution table", name)
	}
	d.name = name
	return d
}

// packageFile returns the one package of the distribution in /pkgs.
func (d distro) packageFile(t *testing.T) string {
	t.Helper()
	matches, err := filepath.Glob(d.pattern)
	if err != nil || len(matches) != 1 {
		t.Fatalf("%s = %v, %v, want one package", d.pattern, matches, err)
	}
	return matches[0]
}

// result is the exit status and the combined output of a command.
type result struct {
	code int
	out  string
}

// run runs argv with stdin as root, or with cred when not nil, and the
// environment of a login of that user.
func run(t *testing.T, cred *syscall.Credential, stdin string, argv ...string) result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C.UTF-8"}
	if cred != nil {
		account, err := user.LookupId(strconv.Itoa(int(cred.Uid)))
		if err != nil {
			t.Fatal(err)
		}
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: cred}
		cmd.Env = append(cmd.Env, "HOME="+account.HomeDir, "USER="+account.Username, "LOGNAME="+account.Username)
	}
	cmd.Dir = "/"
	cmd.Stdin = strings.NewReader(stdin)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return result{0, out.String()}
	case errors.As(err, &exit) && exit.ExitCode() >= 0:
		return result{exit.ExitCode(), out.String()}
	default:
		t.Fatalf("%s: %v\n%s", strings.Join(argv, " "), err, out.String())
		return result{}
	}
}

// mustRun runs argv as root and stops the test unless it exits 0.
func mustRun(t *testing.T, stdin string, argv ...string) string {
	t.Helper()
	r := run(t, nil, stdin, argv...)
	if r.code != 0 {
		t.Fatalf("%s exited %d:\n%s", strings.Join(argv, " "), r.code, r.out)
	}
	return r.out
}

// credential returns the ids of a user of the image.
func credential(t *testing.T, name string) *syscall.Credential {
	t.Helper()
	account, err := user.Lookup(name)
	if err != nil {
		t.Fatal(err)
	}
	uid, err := strconv.Atoi(account.Uid)
	if err != nil {
		t.Fatal(err)
	}
	gid, err := strconv.Atoi(account.Gid)
	if err != nil {
		t.Fatal(err)
	}
	return &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}
}

// serviceGID returns the group slendmail the package created.
func serviceGID(t *testing.T) int {
	t.Helper()
	group, err := user.LookupGroup("slendmail")
	if err != nil {
		t.Fatal(err)
	}
	gid, err := strconv.Atoi(group.Gid)
	if err != nil {
		t.Fatal(err)
	}
	return gid
}

// install installs the package of d as root and checks the exit status, the
// output and the binary.
func install(t *testing.T, d distro) {
	t.Helper()
	out := mustRun(t, "", append(slices.Clone(d.install), d.packageFile(t))...)
	t.Logf("install:\n%s", out)
	checkInstallerOutput(t, out)
	checkMode(t, binary, 0, serviceGID(t), 0o755|os.ModeSetgid)
}

// checkInstallerOutput fails on a line of a package manager or a package
// script that reports an error or a warning.
func checkInstallerOutput(t *testing.T, out string) {
	t.Helper()
	for line := range strings.Lines(out) {
		lower := strings.ToLower(line)
		if strings.Contains(lower, "error") || strings.Contains(lower, "warning") || strings.Contains(lower, "no such file") {
			t.Errorf("installer reported %q", strings.TrimSpace(line))
		}
	}
}

// checkMode checks owner, group and mode of path, not following a link.
func checkMode(t *testing.T, path string, uid, gid int, mode os.FileMode) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Error(err)
		return
	}
	stat := info.Sys().(*syscall.Stat_t)
	if int(stat.Uid) != uid || int(stat.Gid) != gid || info.Mode() != mode {
		t.Errorf("%s: %d:%d %v, want %d:%d %v", path, stat.Uid, stat.Gid, info.Mode(), uid, gid, mode)
	}
}

// writeConfig replaces the configuration, root:slendmail 0640 as the
// package installs it.
func writeConfig(t *testing.T, content string) {
	t.Helper()
	if err := os.WriteFile(config, []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(config, 0, serviceGID(t)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(config, 0o640); err != nil {
		t.Fatal(err)
	}
}

// heldConfig has no target, so every message is held with exit status 78;
// the comment makes it differ from the file of the package.
const heldConfig = "# smoke: no target, messages are held\n"

// workingConfig sends a message to the hook when hookRecipient receives
// it, every other one to the receiver as ntfy.
func workingConfig(receiverURL, hook string) string {
	return fmt.Sprintf(`[target.local]
type = "ntfy"
url = %q

[target.hook]
type = "exec"
argv = [%q]

[[route]]
recipient = %q
targets = ["hook"]

[[route]]
targets = ["local"]
`, receiverURL+"/smoke", hook, hookRecipient)
}

// fixtures writes the messages and the hook into a directory every user
// reads and returns it. The hook writes its groups and its group to the
// FIFO hook.fifo beside it when that exists.
func fixtures(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "smoke-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(dir, "hook.fifo")
	hook := fmt.Sprintf("#!/bin/sh\nif [ -p %s ]; then echo \"$(id -G) / $(id -g)\" > %s; fi\n", fifo, fifo)
	for name, file := range map[string]struct {
		data []byte
		mode os.FileMode
	}{
		"attachment.eml": {attachmentEML, 0o644},
		"hook.eml":       {hookEML, 0o644},
		"hook":           {[]byte(hook), 0o755},
	} {
		if err := os.WriteFile(filepath.Join(dir, name), file.data, file.mode); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// spoolMessages returns the messages in an area of the spool.
func spoolMessages(t *testing.T, area string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(spoolDir, area, "*.eml"))
	if err != nil {
		t.Fatal(err)
	}
	return matches
}

// allSpoolMessages returns the messages in queue/, hold/ and failed/.
func allSpoolMessages(t *testing.T) []string {
	t.Helper()
	var all []string
	for _, area := range []string{"queue", "hold", "failed"} {
		all = append(all, spoolMessages(t, area)...)
	}
	return all
}

// listSpool returns ls -lR of the spool and the state of its entries for
// a failure report.
func listSpool(t *testing.T) string {
	t.Helper()
	list := run(t, nil, "", "ls", "-lnR", spoolDir).out
	sidecars, _ := filepath.Glob(filepath.Join(spoolDir, "*", "*.json"))
	for _, sidecar := range sidecars {
		data, err := os.ReadFile(sidecar)
		list += fmt.Sprintf("%s: %s %v\n", sidecar, data, err)
	}
	return list
}

// event is one request of slendmail to the receiver: the publish of a text
// (POST) or of an attachment (PUT).
type event struct {
	method, title, message, filename string
}

// receiver is an ntfy server that hands each request to the channel of the
// marker it contains, in the title or the text, and answers 503 once for a
// marker set to fail.
type receiver struct {
	*httptest.Server
	mu        sync.Mutex
	waiting   map[string]chan event
	failing   map[string]bool
	unmatched []event
}

func newReceiver(t *testing.T) *receiver {
	t.Helper()
	r := &receiver{waiting: map[string]chan event{}, failing: map[string]bool{}}
	r.Server = httptest.NewServer(http.HandlerFunc(r.serve))
	t.Cleanup(r.Close)
	return r
}

func (r *receiver) serve(w http.ResponseWriter, req *http.Request) {
	ev := event{method: req.Method, title: req.URL.Query().Get("title"), filename: req.URL.Query().Get("filename")}
	if req.Method == http.MethodPost {
		var publish struct{ Title, Message string }
		if err := json.NewDecoder(req.Body).Decode(&publish); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		ev.title, ev.message = publish.Title, publish.Message
	} else {
		_, _ = io.Copy(io.Discard, req.Body)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for marker, ch := range r.waiting {
		if !strings.Contains(ev.title, marker) && !strings.Contains(ev.message, marker) {
			continue
		}
		if r.failing[marker] && req.Method == http.MethodPost {
			delete(r.failing, marker)
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		select {
		case ch <- ev:
		default:
		}
		_, _ = io.WriteString(w, "{}")
		return
	}
	r.unmatched = append(r.unmatched, ev)
	_, _ = io.WriteString(w, "{}")
}

// expect returns the channel of the requests that carry marker.
func (r *receiver) expect(marker string) <-chan event {
	r.mu.Lock()
	defer r.mu.Unlock()
	ch := make(chan event, 16)
	r.waiting[marker] = ch
	return ch
}

// failOnce makes the first publish of marker fail with 503.
func (r *receiver) failOnce(marker string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failing[marker] = true
}

// report lists the requests no marker claimed.
func (r *receiver) report() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return fmt.Sprintf("requests without a marker: %+v", r.unmatched)
}

// isEmpty tells that ch holds no request now; slendmail sends before it
// exits, so after its exit nothing more arrives for its message.
func isEmpty(ch <-chan event) bool {
	select {
	case <-ch:
		return false
	default:
		return true
	}
}

// waitEvent returns the next request on ch, or stops the test with report
// when the deadline passes.
func waitEvent(t *testing.T, ch <-chan event, deadline time.Time, what string, report func() string) event {
	t.Helper()
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	select {
	case ev := <-ch:
		return ev
	case <-ctx.Done():
		t.Fatalf("%s did not arrive by %s\n%s", what, deadline.Format(time.TimeOnly), report())
		return event{}
	}
}

// syncBuffer collects the output of a daemon while the test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// startDaemon runs argv in the foreground until the test ends and returns
// its collected output.
func startDaemon(t *testing.T, argv ...string) *syncBuffer {
	t.Helper()
	out := &syncBuffer{}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return out
}

// openFIFO creates a FIFO every user may write and holds it open for
// reading and writing, so that a writer never waits for a reader; its
// lines arrive on the returned channel until the test ends.
func openFIFO(t *testing.T, path string) <-chan string {
	t.Helper()
	if err := syscall.Mkfifo(path, 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	lines := make(chan string, 16)
	go func() {
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			select {
			case lines <- scanner.Text():
			default:
			}
		}
	}()
	t.Cleanup(func() {
		_ = file.Close()
		_ = os.Remove(path)
	})
	return lines
}

// TestSmokeSetgid runs the setgid tests of internal/app against the binary,
// user, group and spool of the installed package: the binary executes
// itself exactly once, ignores the environment of an attacker, lays out
// spool entries with the right owners and runs a hook without its group.
func TestSmokeSetgid(t *testing.T) {
	d := currentDistro(t)
	t.Run("T-PKG-17/setgid-tests-installed", func(t *testing.T) {
		install(t, d)
		r := run(t, nil, "", "/e2e/app.test", "-test.run", "^TestSetgid", "-test.v", "-installed")
		t.Logf("app.test:\n%s", r.out)
		if r.code != 0 {
			t.Errorf("setgid tests against the package exited %d", r.code)
		}
		// A test binary without the setgid_e2e tag or with renamed tests
		// would match nothing and exit 0.
		for _, name := range []string{"TestSetgidReexec", "TestSetgidSpool", "TestSetgidHookDropsGroup"} {
			if !strings.Contains(r.out, "--- PASS: "+name+" ") {
				t.Errorf("app.test did not pass %s", name)
			}
		}
	})
}
