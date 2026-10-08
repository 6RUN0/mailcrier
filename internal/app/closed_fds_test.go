package app

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

// closedFdsHelperConfig carries the configuration of the closed fds
// helper process; set, it makes TestClosedFdsHelper run.
const closedFdsHelperConfig = "MAILCRIER_CLOSED_FDS_HELPER"

// closedFdsMessage is the message the closed fds helper delivers.
const closedFdsMessage = "Subject: cron\n\nline 1\nline 2\n"

// TestClosedFdsHelper is not a test: it is the child process of
// TestRunClosedStdoutStderr, started with fds 1 and 2 closed. It reports
// where fds 1 and 2 point before and after Run, then the log, on fd 3.
func TestClosedFdsHelper(t *testing.T) {
	config := os.Getenv(closedFdsHelperConfig)
	if config == "" {
		return
	}
	report := os.NewFile(3, "report")
	reportFds := func(when string) {
		for _, fd := range []int{1, 2} {
			target, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", fd))
			_, _ = fmt.Fprintf(report, "%s fd%d %s %v\n", when, fd, target, err)
		}
	}
	reportFds("before")
	// The log goes through stderr as well, so a write to the closed
	// descriptor would show as a failed or crashed run.
	logOutput := io.MultiWriter(report, os.Stderr)
	deps := Deps{
		NewLogger:      func(string) *slog.Logger { return slog.New(slog.NewTextHandler(logOutput, nil)) },
		ConfigFS:       fstest.MapFS{SystemConfigPath: {Data: []byte(config)}},
		ConfigPath:     SystemConfigPath,
		HTTP:           &http.Client{},
		Hostname:       "host1.example.org",
		Now:            time.Now,
		Program:        "/usr/sbin/sendmail",
		Stdout:         os.Stdout,
		Stderr:         os.Stderr,
		SetLogOutput:   func(io.Writer) {},
		LookupUserName: func(int) (string, bool) { return "", false },
	}
	code := Run(t.Context(), deps, []string{"root"}, strings.NewReader(closedFdsMessage))
	reportFds("after")
	os.Exit(code)
}

// TestRunClosedStdoutStderr pins that a caller which closes stdout and
// stderr, as atd and some daemons do, still gets the message delivered and
// exit status 0: the Go runtime reopens fds 0-2 on /dev/null before main,
// so no file of the process takes their place.
func TestRunClosedStdoutStderr(t *testing.T) {
	t.Run("T-CALL-28/closed-stdout-stderr", func(t *testing.T) {
		if runtime.GOOS != "linux" {
			t.Skip("reads /proc/self/fd")
		}
		hook := writeHookScript(t, "cat > \"$0.stdin\"\n")
		reportR, reportW, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = reportR.Close() }()
		// os/exec gives a nil Stdout or Stderr /dev/null, never a closed
		// descriptor, so the shell closes them before it execs the helper.
		cmd := exec.Command("/bin/sh", "-c", `exec "$0" "$@" >&- 2>&-`, os.Args[0], "-test.run=^TestClosedFdsHelper$")
		cmd.Env = append(os.Environ(), closedFdsHelperConfig+"=[target.run]\ntype = \"exec\"\nargv = [\""+hook+"\"]\n")
		cmd.ExtraFiles = []*os.File{reportW}
		// The shell gets a file, not /dev/null, so fds of the helper that
		// point to /dev/null show the close and the reopen.
		output, err := os.Create(hook + ".output")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = output.Close() }()
		cmd.Stdout, cmd.Stderr = output, output
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		_ = reportW.Close()
		report, readErr := io.ReadAll(reportR)
		waitErr := cmd.Wait()
		if readErr != nil || waitErr != nil {
			t.Fatalf("helper: read %v, exit %v; report:\n%s", readErr, waitErr, report)
		}
		for _, want := range []string{
			"before fd1 /dev/null <nil>\n", "before fd2 /dev/null <nil>\n",
			"after fd1 /dev/null <nil>\n", "after fd2 /dev/null <nil>\n",
			`msg="message received"`,
		} {
			if !strings.Contains(string(report), want) {
				t.Errorf("report lacks %q:\n%s", want, report)
			}
		}
		if got, err := os.ReadFile(hook + ".stdin"); err != nil || string(got) != closedFdsMessage {
			t.Errorf("hook got %q, %v; want the message", got, err)
		}
		if got, err := os.ReadFile(hook + ".output"); err != nil || len(got) != 0 {
			t.Errorf("output of the helper %q, %v; want none", got, err)
		}
	})
}
