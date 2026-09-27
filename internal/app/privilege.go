package app

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/6RUN0/slendmail/internal/sendmail"
)

// selfExe is the running binary. Executing it starts the same inode even
// after a package upgrade replaced the file, and the setgid bit of that
// inode applies again.
const selfExe = "/proc/self/exe"

// installedPath is where packages put the binary; the re-exec falls back to
// it when /proc is not mounted. Builds for another layout set it with
// -ldflags "-X github.com/6RUN0/slendmail/internal/app.installedPath=...".
var installedPath = "/usr/sbin/slendmail"

// Credentials are the ids of the process that decide whether it runs with
// the group privilege of a setgid binary.
type Credentials struct {
	// UID and GID are the real user and group of the caller.
	UID, GID int
	// EGID is the effective group; it differs from GID when the kernel
	// applied the setgid bit of the binary.
	EGID int
	// ServiceUID is the uid of the slendmail system user, -1 when the
	// system has none.
	ServiceUID int
}

// isElevated reports whether the setgid bit gave the process a group the
// caller does not have. root is not elevated: it can read the files of the
// group anyway, so nothing it passes in crosses a privilege boundary.
func (c Credentials) isElevated() bool {
	return c.EGID != c.GID && c.UID != 0
}

// isPrivilegedCaller reports whether the caller may use the service modes
// that read the whole configuration or send to every target.
func (c Credentials) isPrivilegedCaller() bool {
	return !c.isElevated() || c.UID == c.ServiceUID
}

// validTZ accepts zone names such as "UTC" or "Europe/Berlin". A leading
// slash, a leading colon or ".." would make the time package open an
// arbitrary file with the group privilege.
var validTZ = regexp.MustCompile(`^[A-Za-z0-9_+-]+(/[A-Za-z0-9_+-]+)*$`)

// sanitizeEnv returns the part of env an elevated process keeps: USER,
// LOGNAME, HOME, LANG, LC_*, a zone name in TZ, and GOTRACEBACK=none, which
// the Go runtime itself adds in secure mode. Entries without "=" are
// dropped, and of repeated keys only the first kept entry survives.
//
// Everything else goes, because a static Go binary gets no help from the
// dynamic loader: GODEBUG, proxy and TLS variables act on the process with
// the group privilege, and some are read in package init, before main.
// The result is stable under a second pass, so the re-executed process
// sees nothing to remove and does not execute itself again.
func sanitizeEnv(env []string) []string {
	kept := make([]string, 0, len(env))
	seen := map[string]bool{}
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || seen[key] || !isAllowedEnv(key, value) {
			continue
		}
		seen[key] = true
		kept = append(kept, entry)
	}
	return kept
}

func isAllowedEnv(key, value string) bool {
	switch {
	case key == "USER", key == "LOGNAME", key == "HOME", key == "LANG":
		return true
	case strings.HasPrefix(key, "LC_"):
		return true
	case key == "TZ":
		return validTZ.MatchString(value)
	case key == "GOTRACEBACK":
		return value == "none"
	default:
		return false
	}
}

// Process is what main knows about the process before anything else runs.
type Process struct {
	// Argv is the whole command line, argv[0] included.
	Argv []string
	// Environ is the environment, as os.Environ.
	Environ []string
	// Credentials need only UID, GID and EGID here.
	Credentials Credentials
	// Exec replaces the process image, as syscall.Exec; it returns only
	// on failure.
	Exec func(path string, argv, env []string) error
	// ReplaceEnv makes env the whole environment of the process.
	ReplaceEnv func(env []string)
}

// Harden runs first in main, before anything reads the environment or
// formats a time (TZ names a file the time package opens). An elevated
// process whose environment holds anything sanitizeEnv removes executes
// itself with the sanitized environment and does not return. When both
// exec attempts fail, Harden replaces the environment in place and
// returns the error; Run then works in a restricted mode.
//
// The returned arguments, argv[0] excluded, go to Run. They start with
// sendmail.MarkerEnvConfig when SLENDMAIL_CONFIG was set, so that the
// process after the exec can warn about the variable it no longer sees. A
// forged marker yields one extra warning, nothing else.
func Harden(p Process) (args []string, reexecErr error) {
	args = p.Argv[1:]
	if !p.Credentials.isElevated() {
		return args, nil
	}
	env := sanitizeEnv(p.Environ)
	if slices.Equal(env, p.Environ) {
		return args, nil
	}
	if _, ok := lookupEnv(p.Environ, envConfig); ok {
		args = append([]string{sendmail.MarkerEnvConfig}, args...)
	}
	argv := append([]string{p.Argv[0]}, args...)
	var errs []error
	for _, path := range []string{selfExe, installedPath} {
		err := p.Exec(path, argv, env)
		errs = append(errs, fmt.Errorf("exec %s: %w", path, err))
	}
	p.ReplaceEnv(env)
	return args, errors.Join(errs...)
}

// withoutHTTP2 returns a copy of rt that speaks HTTP/1.1 only. The HTTP/2
// transport decides in package init, from GODEBUG, whether to print every
// request header, the token-bearing path among them; a process that could
// not re-execute itself with a clean environment must not reach that code.
// A RoundTripper that is not an *http.Transport is returned unchanged.
func withoutHTTP2(rt http.RoundTripper) http.RoundTripper {
	var transport *http.Transport
	switch t := rt.(type) {
	case nil:
		transport = http.DefaultTransport.(*http.Transport).Clone()
	case *http.Transport:
		transport = t.Clone()
	default:
		return rt
	}
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	transport.Protocols = protocols
	transport.ForceAttemptHTTP2 = false
	transport.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	// A TLS configuration that already offers "h2" in ALPN lets the server
	// switch to HTTP/2, which this transport then cannot speak.
	if transport.TLSClientConfig == nil {
		transport.TLSClientConfig = &tls.Config{}
	} else {
		transport.TLSClientConfig = transport.TLSClientConfig.Clone()
	}
	transport.TLSClientConfig.NextProtos = []string{"http/1.1"}
	return transport
}

// lookupEnv returns the value of the first entry for key, as getenv does.
func lookupEnv(env []string, key string) (string, bool) {
	for _, entry := range env {
		if k, v, ok := strings.Cut(entry, "="); ok && k == key {
			return v, true
		}
	}
	return "", false
}
