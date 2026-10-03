package app

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"testing/iotest"
	"time"

	"github.com/6RUN0/mailcrier/internal/sendmail"
)

// Credentials of the cases below. The mailcrier user and group are 990; a
// caller runs the setgid binary with its own uid and gid and egid 990.
var (
	elevatedUser    = Credentials{UID: 1000, GID: 1000, EGID: 990, ServiceUID: 990}
	elevatedRoot    = Credentials{UID: 0, GID: 0, EGID: 990, ServiceUID: 990}
	elevatedService = Credentials{UID: 990, GID: 100, EGID: 990, ServiceUID: 990}
	plainUser       = Credentials{UID: 1000, GID: 1000, EGID: 1000, ServiceUID: 990}
)

func TestSanitizeEnv(t *testing.T) {
	cases := []struct {
		name string
		env  []string
		want []string
	}{
		{"allowlist-kept-in-order", []string{"LANG=C.UTF-8", "USER=alice", "LOGNAME=alice", "HOME=/home/alice", "LC_ALL=C", "TZ=Europe/Berlin"}, []string{"LANG=C.UTF-8", "USER=alice", "LOGNAME=alice", "HOME=/home/alice", "LC_ALL=C", "TZ=Europe/Berlin"}},
		{"everything-else-removed", []string{"GODEBUG=http2debug=2", "HTTPS_PROXY=http://evil:3128", "SSL_CERT_FILE=/tmp/ca.pem", "MAILCRIER_CONFIG=/tmp/x", "PATH=/usr/bin", "USER=alice"}, []string{"USER=alice"}},
		{"duplicate-keys-first-wins", []string{"USER=alice", "USER=root", "HOME=/a", "HOME=/b"}, []string{"USER=alice", "HOME=/a"}},
		{"entries-without-equals", []string{"USER", "garbage", "=x", "LANG=C"}, []string{"LANG=C"}},
		{"gotraceback-none-kept", []string{"GOTRACEBACK=none"}, []string{"GOTRACEBACK=none"}},
		{"gotraceback-other-value", []string{"GOTRACEBACK=all", "GOTRACEBACK=crash"}, []string{}},
		{"gotraceback-runtime-appended", []string{"GOTRACEBACK=all", "GOTRACEBACK=none"}, []string{"GOTRACEBACK=none"}},
		{"tz-plain-zone", []string{"TZ=UTC"}, []string{"TZ=UTC"}},
		{"tz-absolute-path", []string{"TZ=/etc/shadow"}, []string{}},
		{"tz-colon-path", []string{"TZ=:/etc/shadow"}, []string{}},
		{"tz-parent-dir", []string{"TZ=../../etc/shadow"}, []string{}},
		{"tz-dots-inside", []string{"TZ=Europe/../../x"}, []string{}},
		{"tz-invalid-then-valid", []string{"TZ=/x", "TZ=Asia/Tokyo"}, []string{"TZ=Asia/Tokyo"}},
		{"empty", nil, []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeEnv(tc.env)
			if !slices.Equal(got, tc.want) {
				t.Errorf("sanitizeEnv(%q) = %q, want %q", tc.env, got, tc.want)
			}
			if again := sanitizeEnv(got); !slices.Equal(again, got) {
				t.Errorf("second pass changed %q to %q", got, again)
			}
		})
	}
}

// FuzzSanitize checks the property that stops a re-exec loop: a sanitized
// environment passes a second sanitize unchanged. It also checks that the
// result holds only allowed entries, each key once.
func FuzzSanitize(f *testing.F) {
	for _, seed := range []string{
		"USER=a\x00GODEBUG=http2debug=2\x00TZ=/etc/shadow",
		"GOTRACEBACK=all\x00GOTRACEBACK=none\x00USER=a\x00USER=b",
		"TZ=../x\x00TZ=UTC\x00LC_ALL=C\x00noequals",
		"",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, joined string) {
		env := strings.Split(joined, "\x00")
		once := sanitizeEnv(env)
		if twice := sanitizeEnv(once); !slices.Equal(once, twice) {
			t.Fatalf("sanitizeEnv not idempotent: %q -> %q -> %q", env, once, twice)
		}
		seen := map[string]bool{}
		for _, entry := range once {
			key, value, ok := strings.Cut(entry, "=")
			if !ok || seen[key] || !isAllowedEnv(key, value) {
				t.Fatalf("sanitizeEnv(%q) kept %q", env, entry)
			}
			seen[key] = true
		}
	})
}

// countingServer accepts every request and counts them per protocol.
type countingServer struct {
	*httptest.Server
	protos chan string
}

func newCountingServer(t *testing.T, tlsHTTP2 bool) *countingServer {
	t.Helper()
	s := &countingServer{protos: make(chan string, 8)}
	s.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		s.protos <- r.Proto
	}))
	if tlsHTTP2 {
		s.EnableHTTP2 = true
		s.StartTLS()
	} else {
		s.Start()
	}
	t.Cleanup(s.Close)
	return s
}

// received returns the protocols of the requests that arrived so far.
func (s *countingServer) received() []string {
	var protos []string
	for {
		select {
		case proto := <-s.protos:
			protos = append(protos, proto)
		default:
			return protos
		}
	}
}

// TestRunElevatedIgnoresConfigOverride pins that neither --config nor
// MAILCRIER_CONFIG lets a caller of the setgid binary pick the
// configuration: the default one is used and a warning names the source.
func TestRunElevatedIgnoresConfigOverride(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		environ []string
		source  string
	}{
		{"option", []string{"--config", "/tmp/evil.conf", "-ti"}, []string{"USER=alice"}, "--config"},
		{"option-with-equals", []string{"--config=/tmp/evil.conf"}, []string{"USER=alice"}, "--config"},
		{"environment", []string{"-ti"}, []string{"USER=alice", "MAILCRIER_CONFIG=/tmp/evil.conf"}, "MAILCRIER_CONFIG"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			good, evil := newCountingServer(t, false), newCountingServer(t, false)
			inv := &invocation{
				config:  httpTargetConfig(good.URL),
				files:   fstest.MapFS{"tmp/evil.conf": {Data: []byte(httpTargetConfig(evil.URL))}},
				args:    tc.args,
				environ: tc.environ,
				creds:   elevatedUser,
				stdin:   strings.NewReader("Subject: t\n\nb\n"),
			}
			if code := inv.run(t); code != 0 {
				t.Fatalf("Run() = %d, want 0; output:\n%s", code, inv.output())
			}
			if len(good.received()) != 1 || len(evil.received()) != 0 {
				t.Error("message did not go to the target of the default configuration only")
			}
			want := `level=WARN msg="configuration override ignored" source=` + tc.source
			if log := inv.log("mailcrier"); strings.Count(log, want) != 1 {
				t.Errorf("log does not warn once with %q:\n%s", want, log)
			}
		})
	}
}

// TestRunHonoursConfigOverride covers the unelevated process, such as a
// container without the setgid bit: --config wins over MAILCRIER_CONFIG,
// which wins over the default.
func TestRunHonoursConfigOverride(t *testing.T) {
	cases := []struct {
		name    string
		creds   Credentials
		args    []string
		environ []string
		want    string
	}{
		{"environment", plainUser, nil, []string{"MAILCRIER_CONFIG=/srv/app/env.conf"}, "env"},
		{"option-over-environment", plainUser, []string{"--config", "/srv/app/opt.conf"}, []string{"MAILCRIER_CONFIG=/srv/app/env.conf"}, "opt"},
		{"root-with-setgid-bit", elevatedRoot, []string{"--config=/srv/app/opt.conf"}, nil, "opt"},
		{"default", plainUser, nil, nil, "default"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			servers := map[string]*countingServer{}
			files := fstest.MapFS{}
			for _, name := range []string{"env", "opt"} {
				servers[name] = newCountingServer(t, false)
				files["srv/app/"+name+".conf"] = &fstest.MapFile{Data: []byte(httpTargetConfig(servers[name].URL))}
			}
			servers["default"] = newCountingServer(t, false)
			inv := &invocation{
				config:  httpTargetConfig(servers["default"].URL),
				files:   files,
				args:    tc.args,
				environ: tc.environ,
				creds:   tc.creds,
				stdin:   strings.NewReader("Subject: t\n\nb\n"),
			}
			if code := inv.run(t); code != 0 {
				t.Fatalf("Run() = %d, want 0; output:\n%s", code, inv.output())
			}
			for name, server := range servers {
				if got, want := len(server.received()), map[bool]int{true: 1}[name == tc.want]; got != want {
					t.Errorf("receiver of %s got %d requests, want %d", name, got, want)
				}
			}
			if strings.Contains(inv.output(), "override ignored") {
				t.Errorf("override reported as ignored:\n%s", inv.output())
			}
		})
	}
}

// TestRunRefusesServiceModes pins who may use --probe and --check-config,
// decided before stdin is read: an elevated caller other than root and the
// mailcrier user gets 77, and an elevated caller that names another
// configuration gets 64 instead of a report on the default one.
func TestRunRefusesServiceModes(t *testing.T) {
	const overrideRefused = " is ignored for a setgid-elevated caller\n"
	const probed = "target=hook class=ok\n"
	cases := []struct {
		name       string
		creds      Credentials
		args       []string
		want       int
		wantStderr string
		wantStdout string
	}{
		{"user-probe", elevatedUser, []string{"--probe"}, 77, "mailcrier: --probe: permission denied\n", ""},
		{"user-check-config", elevatedUser, []string{"--check-config"}, 77, "mailcrier: --check-config: permission denied\n", ""},
		{"root-probe", elevatedRoot, []string{"--probe"}, 0, "", probed},
		{"root-check-config", elevatedRoot, []string{"--check-config"}, 0, checkSummaryClean, ""},
		{"service-user-probe", elevatedService, []string{"--probe"}, 0, "", probed},
		{"service-user-check-config", elevatedService, []string{"--check-config"}, 0, checkSummaryClean, ""},
		{"unelevated-probe", plainUser, []string{"--probe"}, 0, "", probed},
		{"unelevated-check-config", plainUser, []string{"--check-config"}, 0, checkSummaryClean, ""},
		{"service-user-config-option-probe", elevatedService, []string{"--probe", "--config", "/etc/other.conf"}, 64, "mailcrier: --config" + overrideRefused, ""},
		{"service-user-config-option-check-config", elevatedService, []string{"--check-config", "--config", "/etc/other.conf"}, 64, "mailcrier: --config" + overrideRefused, ""},
		{"service-user-config-variable-probe", elevatedService, []string{sendmail.MarkerEnvConfig, "--probe"}, 64, "mailcrier: MAILCRIER_CONFIG" + overrideRefused, ""},
		{"service-user-config-variable-check-config", elevatedService, []string{sendmail.MarkerEnvConfig, "--check-config"}, 64, "mailcrier: MAILCRIER_CONFIG" + overrideRefused, ""},
		{"unelevated-marker-probe", plainUser, []string{sendmail.MarkerEnvConfig, "--probe"}, 0, "", probed},
		{"unelevated-marker-check-config", plainUser, []string{sendmail.MarkerEnvConfig, "--check-config"}, 0, checkSummaryClean, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := newStatusServer(t, http.StatusOK, 0)
			inv := &invocation{
				config: httpTargetConfig(server.URL),
				args:   tc.args,
				creds:  tc.creds,
				stdin:  iotest.ErrReader(fmt.Errorf("stdin read")),
			}
			if code := inv.run(t); code != tc.want {
				t.Fatalf("Run() = %d, want %d; output:\n%s", code, tc.want, inv.output())
			}
			if got := inv.stderr.String(); got != tc.wantStderr {
				t.Errorf("stderr = %q, want %q", got, tc.wantStderr)
			}
			if got := inv.stdout.String(); got != tc.wantStdout {
				t.Errorf("stdout = %q, want %q", got, tc.wantStdout)
			}
		})
	}
}

func TestRunIgnoresAltConfigOption(t *testing.T) {
	server := newCountingServer(t, false)
	for _, args := range [][]string{{"-C", "/tmp/evil.cf", "-ti"}, {"-C/tmp/evil.cf"}} {
		inv := &invocation{config: httpTargetConfig(server.URL), args: args, stdin: strings.NewReader("Subject: t\n\nb\n")}
		if code := inv.run(t); code != 0 {
			t.Fatalf("Run(%q) = %d, want 0", args, code)
		}
		if log := inv.log("mailcrier"); !strings.Contains(log, `level=WARN msg="option ignored" option=-C`) {
			t.Errorf("Run(%q) log lacks the warning:\n%s", args, log)
		}
	}
}

func TestRunRejectsConfigOptionWithoutValue(t *testing.T) {
	for _, args := range [][]string{{"--config"}, {"--config="}, {"--config", ""}} {
		inv := &invocation{config: httpTargetConfig("http://127.0.0.1:1"), args: args, stdin: strings.NewReader("")}
		if code := inv.run(t); code != 64 {
			t.Errorf("Run(%q) = %d, want 64", args, code)
		}
	}
}

// TestRunReexecs pins when the process executes itself again: only when
// elevated and only when the environment holds something sanitizeEnv
// removes.
func TestRunReexecs(t *testing.T) {
	cases := []struct {
		name       string
		creds      Credentials
		environ    []string
		wantReexec bool
	}{
		{"elevated-dirty", elevatedUser, []string{"USER=alice", "GODEBUG=http2debug=2"}, true},
		{"elevated-clean", elevatedUser, []string{"USER=alice", "GOTRACEBACK=none"}, false},
		{"unelevated-dirty", plainUser, []string{"GODEBUG=http2debug=2"}, false},
		{"root-dirty", elevatedRoot, []string{"GODEBUG=http2debug=2"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := newCountingServer(t, false)
			inv := &invocation{
				config:  httpTargetConfig(server.URL),
				args:    []string{"-ti", "root"},
				environ: tc.environ,
				creds:   tc.creds,
				stdin:   strings.NewReader("Subject: t\n\nb\n"),
			}
			if code := inv.run(t); code != 0 {
				t.Fatalf("Run() = %d, want 0; output:\n%s", code, inv.output())
			}
			if !tc.wantReexec {
				if len(inv.execs) != 0 {
					t.Errorf("Run() executed %v", inv.execs)
				}
				return
			}
			wantEnv := []string{"USER=alice"}
			wantArgv := []string{"/usr/sbin/sendmail", "-ti", "root"}
			if len(inv.execs) != 2 || inv.execs[0].path != "/proc/self/exe" || inv.execs[1].path != "/usr/sbin/mailcrier" {
				t.Fatalf("exec calls = %v, want /proc/self/exe, then /usr/sbin/mailcrier", inv.execs)
			}
			for _, call := range inv.execs {
				if !slices.Equal(call.argv, wantArgv) || !slices.Equal(call.env, wantEnv) {
					t.Errorf("exec %s with argv %q env %q, want %q %q", call.path, call.argv, call.env, wantArgv, wantEnv)
				}
			}
		})
	}
}

// TestRunHardenedModeAfterFailedReexec covers an elevated process that
// cannot execute itself (no /proc, no installed binary): it keeps its
// group and delivers the message, with the caller's environment replaced
// by the sanitized one, the standard log output dropped, HTTP/2 off, and
// an error record.
func TestRunHardenedModeAfterFailedReexec(t *testing.T) {
	server := newCountingServer(t, true)
	inv := &invocation{
		config:  httpTargetConfig(server.URL + "/hook/" + secretToken),
		client:  server.Client(),
		environ: []string{"USER=alice", "GODEBUG=http2debug=2", "HTTPS_PROXY=http://127.0.0.1:1"},
		creds:   elevatedUser,
		stdin:   strings.NewReader("Subject: t\n\nb\n"),
	}
	if code := inv.run(t); code != 0 {
		t.Fatalf("Run() = %d, want 0; output:\n%s", code, inv.output())
	}
	if got := server.received(); !slices.Equal(got, []string{"HTTP/1.1"}) {
		t.Errorf("receiver got %v, want one HTTP/1.1 request", got)
	}
	if !slices.Equal(inv.replacedEnv, []string{"USER=alice"}) {
		t.Errorf("environment replaced with %q, want the sanitized one", inv.replacedEnv)
	}
	if inv.logOutput != io.Discard {
		t.Errorf("standard log output = %T, want io.Discard", inv.logOutput)
	}
	if log := inv.log("mailcrier"); !strings.Contains(log, `level=ERROR msg="reexec failed" err="exec /proc/self/exe: exec: permission denied\nexec /usr/sbin/mailcrier: exec: permission denied"`) {
		t.Errorf("log lacks the reexec failure:\n%s", log)
	}
}

// helperMode selects the role of the test binary started by
// TestHTTP2DebugOutputHidesToken.
const helperMode = "MAILCRIER_TEST_HELPER_MODE"

// TestHTTP2DebugOutputHidesToken runs an invocation in a child process
// started with GODEBUG=http2debug=2, which the HTTP/2 transport reads in
// package init, before any code of mailcrier runs. Against an HTTP/2
// receiver the child must not print the token: elevated with a failed
// re-exec it speaks HTTP/1.1 and drops the log output, unelevated its log
// output is redacted. The unelevated case also proves the debug output is
// on, so the elevated case is not passing by accident.
func TestHTTP2DebugOutputHidesToken(t *testing.T) {
	if mode := os.Getenv(helperMode); mode != "" {
		runHTTP2DebugHelper(mode)
		return
	}
	server := newCountingServer(t, true)
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	cases := []struct {
		mode      string
		wantProto string
		// wantStderr must appear in stderr: the redacted request path when
		// the debug output is on.
		wantStderr string
	}{
		{"elevated", "HTTP/1.1", ""},
		{"unelevated", "HTTP/2.0", `http2: Transport encoding header ":path" = "***"`},
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestHTTP2DebugOutputHidesToken$")
			cmd.Env = append(os.Environ(),
				"GODEBUG=http2debug=2",
				helperMode+"="+tc.mode,
				"MAILCRIER_TEST_URL="+server.URL+"/hook/"+secretToken,
				"MAILCRIER_TEST_CA="+string(ca),
			)
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			if err := cmd.Run(); err != nil {
				t.Fatalf("helper failed: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
			}
			if !strings.Contains(stdout.String(), "exit=0\n") {
				t.Errorf("helper did not deliver; stdout:\n%s", stdout.String())
			}
			if got := server.received(); !slices.Equal(got, []string{tc.wantProto}) {
				t.Errorf("receiver got %v, want one %s request", got, tc.wantProto)
			}
			all := stdout.String() + stderr.String()
			if strings.Contains(all, secretToken) {
				t.Errorf("helper output contains the token:\n%s", all)
			}
			if tc.wantStderr != "" && !strings.Contains(stderr.String(), tc.wantStderr) {
				t.Errorf("stderr lacks %q, so the HTTP/2 debug output is off:\n%s", tc.wantStderr, stderr.String())
			}
			if tc.wantStderr == "" && stderr.Len() != 0 {
				t.Errorf("stderr = %q, want nothing", stderr.String())
			}
		})
	}
}

// runHTTP2DebugHelper is the child side of TestHTTP2DebugOutputHidesToken:
// one Run with the real environment, log package and stderr, the syslog
// stand-in on stdout, and an Exec that always fails.
func runHTTP2DebugHelper(mode string) {
	// Harden clears the environment of an elevated process.
	targetURL := os.Getenv("MAILCRIER_TEST_URL")
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM([]byte(os.Getenv("MAILCRIER_TEST_CA")))
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}, ForceAttemptHTTP2: true}
	creds := plainUser
	if mode == "elevated" {
		creds = elevatedUser
	}
	args, reexecErr := Harden(Process{
		Argv:        []string{"/usr/sbin/sendmail", "-ti"},
		Environ:     os.Environ(),
		Credentials: creds,
		Exec:        func(string, []string, []string) error { return fmt.Errorf("exec: no such file or directory") },
		ReplaceEnv:  replaceEnv,
	})
	deps := Deps{
		NewLogger: func(string) *slog.Logger {
			return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))
		},
		ConfigFS:       fstest.MapFS{SystemConfigPath: {Data: []byte(httpTargetConfig(targetURL))}},
		ConfigPath:     SystemConfigPath,
		HTTP:           &http.Client{Transport: transport},
		Hostname:       "host1.example.org",
		Now:            time.Now,
		Stderr:         os.Stderr,
		SetLogOutput:   log.SetOutput,
		Credentials:    creds,
		Environ:        os.Environ(),
		ReexecErr:      reexecErr,
		LookupUserName: lookupUserName,
	}
	code := Run(context.Background(), deps, args, strings.NewReader("Subject: t\n\nb\n"))
	fmt.Printf("exit=%d\n", code)
}

// TestRunIgnoresForgedEnvConfigMarker pins that the marker Harden adds
// only yields a warning in an elevated process; elsewhere it is ignored.
func TestRunIgnoresForgedEnvConfigMarker(t *testing.T) {
	server := newCountingServer(t, false)
	inv := &invocation{config: httpTargetConfig(server.URL), args: []string{sendmail.MarkerEnvConfig, "-ti"}, creds: plainUser, stdin: strings.NewReader("Subject: t\n\nb\n")}
	if code := inv.run(t); code != 0 {
		t.Fatalf("Run() = %d, want 0", code)
	}
	if strings.Contains(inv.output(), "override ignored") {
		t.Errorf("forged marker produced a warning:\n%s", inv.output())
	}
}
