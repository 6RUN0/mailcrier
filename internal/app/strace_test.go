package app

import (
	"regexp"
	"strings"
	"testing"
)

// tracedExec is a successful execve call in strace -f output: the path
// and the offset of the line that starts the call.
type tracedExec struct {
	path   string
	offset int
}

var (
	execDone     = regexp.MustCompile(`^(\d+) +execve\("([^"]+)".*= 0$`)
	execPending  = regexp.MustCompile(`^(\d+) +execve\("([^"]+)".* <(?:unfinished|pid changed to \d+) \.\.\.>$`)
	execResumed  = regexp.MustCompile(`^(\d+) +<\.\.\. execve resumed>.*= (-?\d+)`)
	execReplaced = regexp.MustCompile(`^(\d+) +\+\+\+ superseded by execve in pid (\d+) \+\+\+$`)
)

// successfulExecs returns the successful execve calls of strace -f output
// in the order they completed. strace splits a call into a line that
// opens it and a resumed line when another thread reports in between, or
// when a thread other than the leader executes: that thread's line ends
// with "<pid changed to N ...>" or "<unfinished ...>", the leader N
// announces it as superseded and resumes the call under its own pid.
func successfulExecs(trace string) []tracedExec {
	var execs []tracedExec
	pending := map[string]tracedExec{}
	offset := 0
	for line := range strings.SplitAfterSeq(trace, "\n") {
		start := offset
		offset += len(line)
		line = strings.TrimSuffix(line, "\n")
		if m := execDone.FindStringSubmatch(line); m != nil {
			execs = append(execs, tracedExec{m[2], start})
		} else if m := execPending.FindStringSubmatch(line); m != nil {
			pending[m[1]] = tracedExec{m[2], start}
		} else if m := execReplaced.FindStringSubmatch(line); m != nil {
			if call, ok := pending[m[2]]; ok {
				delete(pending, m[2])
				pending[m[1]] = call
			}
		} else if m := execResumed.FindStringSubmatch(line); m != nil {
			call, ok := pending[m[1]]
			delete(pending, m[1])
			if ok && m[2] == "0" {
				execs = append(execs, call)
			}
		}
	}
	return execs
}

// TestSuccessfulExecs pins the shapes strace 6.13 of the setgid-e2e image
// printed for a re-exec: the split cases are verbatim traces, of the
// setgid e2e test and of a Go program that executes from a goroutine
// locked to a thread other than the leader.
func TestSuccessfulExecs(t *testing.T) {
	const start = `18    execve("/usr/sbin/slendmail", ["/usr/sbin/slendmail", "-ti"], 0x7ffffa334488 /* 9 vars */) = 0` + "\n" +
		`18    umask(007)                        = 022` + "\n"
	cases := []struct {
		name  string
		trace string
		want  []string
	}{
		{"one line", start +
			`18    execve("/proc/self/exe", ["/usr/sbin/slendmail", "-ti"], 0x20e14a720180 /* 2 vars */) = 0` + "\n",
			[]string{"/usr/sbin/slendmail", "/proc/self/exe"}},
		{"leader split by another thread", start +
			`18    execve("/proc/self/exe", ["/usr/sbin/slendmail", "--ignored-env-config", "--config", "/tmp/setgid-e2e-567597308/evil.c"..., "-ti"], 0x20e14a720180 /* 2 vars */ <unfinished ...>` + "\n" +
			`23    ???( <detached ...>` + "\n" +
			`18    <... execve resumed>)             = 0` + "\n",
			[]string{"/usr/sbin/slendmail", "/proc/self/exe"}},
		{"other thread, pid changed", `11    execve("/t/threadexec", ["/t/threadexec"], 0x7ffc7f13a2a0 /* 4 vars */) = 0` + "\n" +
			`14    execve("/proc/self/exe", ["/t/threadexec", "again"], 0x3a5bdc092048 /* 0 vars */ <pid changed to 11 ...>` + "\n" +
			`11    +++ superseded by execve in pid 14 +++` + "\n" +
			`11    <... execve resumed>)             = 0` + "\n",
			[]string{"/t/threadexec", "/proc/self/exe"}},
		{"other thread, unfinished", `302   execve("/t/threadexec", ["/t/threadexec"], 0x7ffc7bb62930 /* 4 vars */) = 0` + "\n" +
			`304   execve("/proc/self/exe", ["/t/threadexec", "again"], 0x169bd5502028 /* 0 vars */ <unfinished ...>` + "\n" +
			`305   ???( <detached ...>` + "\n" +
			`302   +++ superseded by execve in pid 304 +++` + "\n" +
			`302   <... execve resumed>)             = 0` + "\n",
			[]string{"/t/threadexec", "/proc/self/exe"}},
		{"failed split call", start +
			`18    execve("/nonexistent", ["/nonexistent"], 0x20e14a720180 /* 2 vars */ <unfinished ...>` + "\n" +
			`23    ???( <detached ...>` + "\n" +
			`18    <... execve resumed>)             = -1 ENOENT (No such file or directory)` + "\n",
			[]string{"/usr/sbin/slendmail"}},
		{"failed one line call", start +
			`18    execve("/nonexistent", ["/nonexistent"], 0x20e14a720180 /* 2 vars */) = -1 ENOENT (No such file or directory)` + "\n",
			[]string{"/usr/sbin/slendmail"}},
		{"never resumed", start +
			`18    execve("/proc/self/exe", ["/usr/sbin/slendmail", "-ti"], 0x20e14a720180 /* 2 vars */ <unfinished ...>` + "\n",
			[]string{"/usr/sbin/slendmail"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			execs := successfulExecs(c.trace)
			var got []string
			for _, e := range execs {
				got = append(got, e.path)
				line, _, _ := strings.Cut(c.trace[e.offset:], "\n")
				if !strings.Contains(line, `execve("`+e.path+`"`) {
					t.Errorf("offset %d of %s is not the line that starts the call", e.offset, e.path)
				}
			}
			if strings.Join(got, " ") != strings.Join(c.want, " ") {
				t.Errorf("successfulExecs = %q, want %q", got, c.want)
			}
		})
	}
}
