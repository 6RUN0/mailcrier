// Package hook implements the exec target type: one run of a program per
// message, with the message on its stdin and the envelope in its
// environment. The package is not named exec, the type, because it uses
// os/exec.
package hook

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/6RUN0/slendmail/internal/backend"
)

// DefaultTimeout bounds a run when the target sets no timeout.
const DefaultTimeout = 30 * time.Second

// Limits of the environment of a hook. execve fails with E2BIG past about
// a quarter of the stack limit, and a single value past 128 KiB on Linux
// (MAX_ARG_STRLEN), so a subject of 200 KB would keep the hook from
// starting.
const (
	maxEnvValue = 4 << 10
	maxEnvTotal = 64 << 10
)

// maxOutput bounds what the log record keeps of stdout and stderr; the
// rest is read and dropped, so that a chatty hook does not block.
const maxOutput = 4 << 10

// waitDelay bounds the wait for the pipes after the hook exited or was
// killed: a process that left the process group, or a child that the hook
// left running, may hold them open.
const waitDelay = 2 * time.Second

// exitTempFail is EX_TEMPFAIL of sysexits.h, the status a hook exits with
// to ask for a later attempt.
const exitTempFail = 75

// fixedEnv starts the environment of every hook: nothing of the caller's
// environment reaches the hook but TZ, so that a setgid-elevated process
// passes on nothing its caller set.
var fixedEnv = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C.UTF-8"}

// Process is what the hooks of one invocation share.
type Process struct {
	// Credential sets the user and group of the hook; nil keeps those of
	// the process. A setgid-elevated process sets the real ids, so that
	// the hook does not run with the group of the binary.
	Credential *syscall.Credential
	// TZ is the TZ variable of the process; empty when unset.
	TZ string
	// Log receives the output of the hook, one record per run.
	Log *slog.Logger
}

// Options configure one exec target.
type Options struct {
	// Name is the target name, passed on as SLENDMAIL_TARGET.
	Name string
	// Argv is the command line; Argv[0] is an absolute path. No shell
	// reads it.
	Argv []string
	// Timeout bounds one run; the deadline of the context passed to Send
	// bounds it too. Zero means DefaultTimeout.
	Timeout time.Duration
	// Process holds what all hooks of the invocation share.
	Process Process
}

// Sender runs the hook of one target.
type Sender struct {
	opts Options
}

// New returns a Sender for opts.
func New(opts Options) *Sender {
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultTimeout
	}
	return &Sender{opts: opts}
}

// Caps asks for the message and reports neither a text limit nor files:
// the hook gets the message itself, not the text.
func (s *Sender) Caps() backend.Caps {
	return backend.Caps{CanTakeMessage: true}
}

// Send runs the hook with p.Message on its stdin. Exit status 0 is a
// delivery, 75 a temporary failure, any other status or a signal a
// permanent one. A run past the timeout is a temporary failure; the
// process group of the hook is killed with SIGKILL. A hook that cannot be
// started, for a missing file, a missing permission or an environment
// too big, fails for good.
func (s *Sender) Send(ctx context.Context, p backend.Payload) error {
	if p.Message == nil {
		return &backend.Error{Class: backend.Permanent, Err: errors.New("no message for the hook")}
	}
	ctx, cancel := context.WithTimeout(ctx, s.opts.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.opts.Argv[0], s.opts.Argv[1:]...)
	cmd.Env = buildEnv(s.opts, p.Message)
	cmd.Stdin = bytes.NewReader(p.Message.Raw)
	output := &cappedBuffer{max: maxOutput}
	cmd.Stdout, cmd.Stderr = output, output
	// Pdeathsig kills the hook itself when slendmail dies by SIGKILL,
	// which no handler sees; children of the hook survive that.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Credential: s.opts.Process.Credential, Pdeathsig: syscall.SIGKILL}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = waitDelay
	runErr := start(cmd)
	if runErr == nil {
		runErr = cmd.Wait()
	}
	err := classify(ctx, cmd, runErr)
	s.logOutput(output, err)
	return err
}

// start starts cmd on a locked thread. The kernel sends Pdeathsig when
// the thread that forked the hook exits, not the process; the runtime
// ends a thread only when a goroutine exits while locked to it, so
// unlocking after the fork returns the thread to the pool for good.
func start(cmd *exec.Cmd) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	return cmd.Start()
}

// classify turns the outcome of a run into nil or *backend.Error. A hook
// that exited on its own is judged by its exit status, even when the
// timeout ran out while a child it left behind held its stdout: the
// context only explains a hook that was killed or never started.
func classify(ctx context.Context, cmd *exec.Cmd, err error) error {
	state := cmd.ProcessState
	switch {
	case err == nil:
		return nil
	case state != nil && state.Exited() && state.ExitCode() == 0:
		// The hook succeeded; what it left running holds the pipes.
		return nil
	case state != nil && state.Exited() && state.ExitCode() == exitTempFail:
		return &backend.Error{Class: backend.Temporary, Err: fmt.Errorf("hook asked for a retry: %w", err)}
	case state != nil && state.Exited():
		return &backend.Error{Class: backend.Permanent, Err: fmt.Errorf("hook failed: %w", err)}
	case ctx.Err() != nil:
		return &backend.Error{Class: backend.Temporary, Err: fmt.Errorf("hook killed: %w", context.Cause(ctx))}
	case cmd.Process == nil:
		return &backend.Error{Class: backend.Permanent, Err: fmt.Errorf("hook not started: %w", err)}
	default:
		return &backend.Error{Class: backend.Permanent, Err: fmt.Errorf("hook failed: %w", err)}
	}
}

// logOutput writes what the hook printed as one record, through the
// logger of the process, which masks the secrets of the configuration.
func (s *Sender) logOutput(output *cappedBuffer, err error) {
	if s.opts.Process.Log == nil || output.size == 0 {
		return
	}
	level := slog.LevelInfo
	if err != nil {
		level = slog.LevelWarn
	}
	s.opts.Process.Log.Log(context.Background(), level, "hook output", "target", s.opts.Name, "output", strings.ToValidUTF8(output.String(), ""), "output_size", output.size)
}

// buildEnv returns the environment of a hook: fixedEnv, TZ when the
// process has one, and the SLENDMAIL_* variables of msg. Each value has
// line breaks and NUL replaced by spaces and is cut at a character to
// maxEnvValue bytes, and to what is left of maxEnvTotal for the whole
// environment.
func buildEnv(opts Options, msg *backend.Message) []string {
	env := append([]string{}, fixedEnv...)
	if opts.Process.TZ != "" {
		env = append(env, "TZ="+opts.Process.TZ)
	}
	left := maxEnvTotal
	for _, entry := range env {
		left -= len(entry) + 1
	}
	for _, variable := range []struct{ key, value string }{
		{"SLENDMAIL_SUBJECT", msg.Subject},
		{"SLENDMAIL_FROM", msg.From},
		{"SLENDMAIL_TO", strings.Join(msg.To, ", ")},
		{"SLENDMAIL_HOSTNAME", msg.Hostname},
		{"SLENDMAIL_TARGET", opts.Name},
		{"SLENDMAIL_MSGID", msg.MessageID},
		{"SLENDMAIL_SIZE", strconv.Itoa(len(msg.Raw))},
	} {
		limit := min(maxEnvValue, left-len(variable.key)-2)
		if limit < 0 {
			break
		}
		entry := variable.key + "=" + cutUTF8(envValueCleaner.Replace(variable.value), limit)
		env = append(env, entry)
		left -= len(entry) + 1
	}
	return env
}

// envValueCleaner replaces what would break a value: a line break splits
// it for a hook that reads the environment line by line, and execve
// cannot pass a NUL.
var envValueCleaner = strings.NewReplacer("\r", " ", "\n", " ", "\x00", " ")

// cutUTF8 returns the longest prefix of s of at most limit bytes that does
// not split a character.
func cutUTF8(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// cappedBuffer keeps the first max bytes written to it and counts all of
// them. exec.Cmd, given one writer for stdout and stderr, calls Write from
// one goroutine at a time. The buffer is a field, not embedded: io.Copy
// would call the ReadFrom of an embedded bytes.Buffer and bypass the cap.
type cappedBuffer struct {
	buf  bytes.Buffer
	max  int
	size int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	b.size += len(p)
	if room := b.max - b.buf.Len(); room > 0 {
		b.buf.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

func (b *cappedBuffer) String() string {
	return b.buf.String()
}
