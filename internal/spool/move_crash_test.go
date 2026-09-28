package spool_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/fstest"
	"time"

	"github.com/6RUN0/slendmail/internal/app"
	"github.com/6RUN0/slendmail/internal/message"
	"github.com/6RUN0/slendmail/internal/spool"
)

// Environment of the helper process: the step of Move to die after, the
// spool directory and the configuration.
const (
	helperStep   = "SLENDMAIL_MOVE_HELPER_STEP"
	helperDir    = "SLENDMAIL_MOVE_HELPER_DIR"
	helperConfig = "SLENDMAIL_MOVE_HELPER_CONFIG"
)

// serviceCreds run the queue over every entry, as the slendmail user.
var serviceCreds = app.Credentials{UID: 990, GID: 990, EGID: 990, ServiceUID: 990}

// runQueue runs slendmail -q once in this process with the clock at now.
func runQueue(dir, config string, now time.Time) int {
	deps := app.Deps{
		NewLogger:      func(string) *slog.Logger { return slog.New(slog.NewTextHandler(os.Stderr, nil)) },
		ConfigFS:       fstest.MapFS{app.SystemConfigPath: {Data: []byte(config)}},
		ConfigPath:     app.SystemConfigPath,
		HTTP:           &http.Client{},
		Hostname:       "host1.example.org",
		Now:            func() time.Time { return now },
		Program:        "/usr/sbin/sendmail",
		Stdout:         io.Discard,
		Stderr:         os.Stderr,
		SetLogOutput:   func(io.Writer) {},
		Credentials:    serviceCreds,
		LookupUserName: func(int) (string, bool) { return "", false },
		SpoolDir:       dir,
	}
	return app.Run(context.Background(), deps, []string{"-q"}, strings.NewReader(""))
}

// TestMoveHelper is not a test: it is the child process of
// TestMoveCrash, a queue run that dies with SIGKILL after one step of
// the Move that releases a held message.
func TestMoveHelper(_ *testing.T) {
	step := os.Getenv(helperStep)
	if step == "" {
		return
	}
	spool.SetAfterMoveStep(func(done string) {
		if done == step {
			_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
		}
	})
	os.Exit(runQueue(os.Getenv(helperDir), os.Getenv(helperConfig), time.Now()))
}

// TestMoveCrash kills a queue run after each step of the Move that
// releases a held message into the queue: the next run delivers the
// message once, and after an hour no file of the crash is left.
func TestMoveCrash(t *testing.T) {
	for _, step := range []string{"sidecar", "message"} {
		t.Run(step, func(t *testing.T) {
			var mu sync.Mutex
			var got []string
			receiver := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				var body struct{ Subject string }
				_ = json.NewDecoder(r.Body).Decode(&body)
				mu.Lock()
				got = append(got, body.Subject)
				mu.Unlock()
			}))
			defer receiver.Close()
			config := "[target.a]\ntype = \"http\"\npreset = \"generic-json\"\nurl = \"" + receiver.URL + "/a\"\n"
			dir := t.TempDir()
			sp, err := spool.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			held := spool.NewEntry(spool.NewID(now), 1000, now, now, message.Envelope{}, nil)
			rec, err := sp.Create(spool.HoldDir, held, []byte("Subject: held\n\nbody\n"), spool.Quota{})
			if err != nil {
				t.Fatal(err)
			}
			_ = rec.Close()

			cmd := exec.Command(os.Args[0], "-test.run=^TestMoveHelper$")
			cmd.Env = append(os.Environ(), helperStep+"="+step, helperDir+"="+dir, helperConfig+"="+config)
			output, _ := cmd.CombinedOutput()
			if status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); !ok || status.Signal() != syscall.SIGKILL {
				t.Fatalf("helper ended with %v, want SIGKILL after step %s; output:\n%s", cmd.ProcessState, step, output)
			}
			if len(got) != 0 {
				t.Fatalf("delivered %v before the crash", got)
			}
			if code := runQueue(dir, config, time.Now()); code != 0 {
				t.Fatalf("-q after the crash = %d", code)
			}
			if !slices.Equal(got, []string{"held"}) {
				t.Errorf("delivered %v, want the held message once", got)
			}
			if code := runQueue(dir, config, time.Now().Add(2*time.Hour)); code != 0 {
				t.Fatalf("-q an hour later = %d", code)
			}
			for _, area := range []string{spool.TmpDir, spool.QueueDir, spool.HoldDir, spool.FailedDir} {
				if entries, _ := os.ReadDir(dir + "/" + area); len(entries) != 0 {
					t.Errorf("%s/ keeps %v", area, entries)
				}
			}
		})
	}
}
