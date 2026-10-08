package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/6RUN0/mailcrier/internal/backend"
	"github.com/6RUN0/mailcrier/internal/config"
	"github.com/6RUN0/mailcrier/internal/delivery"
	"github.com/6RUN0/mailcrier/internal/message"
	"github.com/6RUN0/mailcrier/internal/render"
	"github.com/6RUN0/mailcrier/internal/text"
)

// recorder stands in for delivery: it keeps the envelope and the template
// data, once per target, and answers each target with the status set for
// it, OK by default.
type recorder struct {
	statuses map[string]delivery.Status
	calls    int
	env      message.Envelope
	data     []render.Data
}

func (r *recorder) deliver(_ context.Context, targets []delivery.Target, env message.Envelope, d render.Data, _ []message.Attachment) []delivery.Result {
	r.calls++
	r.env = env
	var results []delivery.Result
	for _, target := range targets {
		r.data = append(r.data, d)
		status := r.statuses[target.ID]
		if status == 0 {
			status = delivery.OK
		}
		result := delivery.Result{TargetID: target.ID, Status: status}
		switch status {
		case delivery.Temp:
			result.Err = &backend.Error{Class: backend.Temporary, Status: 503, Err: errors.New("unavailable")}
		case delivery.Perm:
			result.Err = &backend.Error{Class: backend.Permanent, Status: 400, Err: errors.New("rejected")}
		}
		results = append(results, result)
	}
	return results
}

// plainText renders d with the built-in plain template.
func plainText(t *testing.T, d render.Data) string {
	t.Helper()
	tmpl, err := render.Builtin(text.FormatPlain)
	if err != nil {
		t.Fatal(err)
	}
	out, err := tmpl.Execute(d)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// twoTargets configures targets "a" and "b"; the recorder never sends.
const twoTargets = "[target.a]\ntype = \"http\"\npreset = \"generic-json\"\nurl = \"http://127.0.0.1:1/a\"\n\n" +
	"[target.b]\ntype = \"http\"\npreset = \"generic-json\"\nurl = \"http://127.0.0.1:1/b\"\n"

// TestExitStatusValues pins the sysexits.h values the program uses, which
// are its own constants rather than an import.
func TestExitStatusValues(t *testing.T) {
	t.Run("T-MTA-35/sysexits-values", func(t *testing.T) {
		got := []int{exitOK, exitUsage, exitUnavailable, exitNoInput, exitSoftware, exitIOErr, exitTempFail, exitNoPerm, exitConfig}
		want := []int{0, 64, 69, 66, 70, 74, 75, 77, 78}
		if !slices.Equal(got, want) {
			t.Errorf("exit statuses = %v, want %v", got, want)
		}
	})
}

// TestRunExitStatusMatrix has one subtest per row of the exit status
// matrix that applies without a spool, named after the situation, with
// the summary record the row asks for.
func TestRunExitStatusMatrix(t *testing.T) {
	cases := []struct {
		name     string
		statuses map[string]delivery.Status
		want     int
		wantLog  []string
	}{
		{"all-delivered", nil, 0, nil},
		{"delivered-and-suppressed", map[string]delivery.Status{"b": delivery.Suppressed}, 0, []string{`level=INFO msg="target suppressed" target=b`}},
		{"delivered-and-rejected", map[string]delivery.Status{"b": delivery.Perm}, 0, []string{`level=ERROR msg="target failed" target=b class=perm`}},
		{"all-rejected", map[string]delivery.Status{"a": delivery.Perm, "b": delivery.Perm}, 69, []string{`level=ERROR msg="message not delivered"`}},
		{"all-temp-without-spool", map[string]delivery.Status{"a": delivery.Temp, "b": delivery.Temp}, 69, []string{`level=ERROR msg="message lost"`}},
		{"delivered-and-temp-without-spool", map[string]delivery.Status{"a": delivery.Temp}, 0, []string{`level=ERROR msg="message lost for target" target=a`}},
		{"all-suppressed", map[string]delivery.Status{"a": delivery.Suppressed, "b": delivery.Suppressed}, 0, []string{`level=INFO msg="target suppressed" target=a`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recorder{statuses: tc.statuses}
			inv := &invocation{config: twoTargets, stdin: strings.NewReader("Subject: t\n\nb\n"), deliver: rec.deliver}
			if code := inv.run(t); code != tc.want {
				t.Fatalf("Run() = %d, want %d; log:\n%s", code, tc.want, inv.log("mailcrier"))
			}
			log := inv.log("mailcrier")
			for _, want := range tc.wantLog {
				if !strings.Contains(log, want) {
					t.Errorf("log lacks %q:\n%s", want, log)
				}
			}
			isErrorRow := strings.Contains(strings.Join(tc.wantLog, "\n"), "level=ERROR")
			if !isErrorRow && strings.Contains(log, "level=ERROR") {
				t.Errorf("log has an error record:\n%s", log)
			}
		})
	}
	t.Run("configuration-error", func(t *testing.T) {
		inv := &invocation{config: "[target.a]\n", stdin: strings.NewReader("Subject: t\n\nb\n")}
		if code := inv.run(t); code != 78 {
			t.Fatalf("Run() = %d, want 78", code)
		}
	})
	t.Run("unreadable-input", func(t *testing.T) {
		inv := &invocation{config: twoTargets, stdin: iotest.ErrReader(errors.New("input/output error"))}
		if code := inv.run(t); code != 66 {
			t.Fatalf("Run() = %d, want 66", code)
		}
	})
	t.Run("panic-in-run", func(t *testing.T) {
		deliver := func(context.Context, []delivery.Target, message.Envelope, render.Data, []message.Attachment) []delivery.Result {
			panic("bug")
		}
		inv := &invocation{config: twoTargets, stdin: strings.NewReader("Subject: t\n\nb\n"), deliver: deliver}
		if code := inv.run(t); code != 70 {
			t.Fatalf("Run() = %d, want 70", code)
		}
	})
}

// TestRunRejectsCommandLine pins the usage errors: exit 64 with the reason
// on stderr, before stdin is read and without quoting an address.
func TestRunRejectsCommandLine(t *testing.T) {
	t.Run("T-ADJ-12/sender-option-without-value", runRejectsCommandLineCase{[]string{"-t", "-f"}, "option -f requires a value"}.check)
	t.Run("T-MTA-42/line-break-in-sender", runRejectsCommandLineCase{[]string{"-f", "root\nBcc: x@example.org", "-t"}, "line break in the sender address"}.check)
	t.Run("T-MTA-42/line-break-in-recipient", runRejectsCommandLineCase{[]string{"root\r\nBcc: x@example.org"}, "line break in a recipient address"}.check)
	t.Run("T-MTA-33/smtp-mode", runRejectsCommandLineCase{[]string{"-bs"}, "option -bs: the SMTP mode is not supported"}.check)
}

type runRejectsCommandLineCase struct {
	args       []string
	wantStderr string
}

func (tc runRejectsCommandLineCase) check(t *testing.T) {
	t.Helper()
	rec := &recorder{}
	inv := &invocation{config: twoTargets, args: tc.args, stdin: iotest.ErrReader(errors.New("stdin read")), deliver: rec.deliver}
	if code := inv.run(t); code != 64 {
		t.Fatalf("Run() = %d, want 64", code)
	}
	if got := inv.stderr.String(); got != "mailcrier: "+tc.wantStderr+"\n" {
		t.Errorf("stderr = %q", got)
	}
	if strings.Contains(inv.output(), "x@example.org") {
		t.Errorf("output quotes the address:\n%s", inv.output())
	}
	if rec.calls != 0 {
		t.Error("message delivered")
	}
}

// TestRunModes covers the modes that read no message.
func TestRunModes(t *testing.T) {
	t.Run("T-MTA-32/newaliases", runModesCase{"/usr/bin/newaliases", nil, 0, ""}.check)
	t.Run("T-MTA-32/bi", runModesCase{"", []string{"-bi"}, 0, ""}.check)
	t.Run("T-MTA-32/capital-i", runModesCase{"", []string{"-I"}, 0, ""}.check)
	t.Run("T-MTA-31/mailq", runModesCase{"/usr/bin/mailq", nil, 0, "queue is empty\n"}.check)
	t.Run("T-MTA-31/bp", runModesCase{"", []string{"-bp"}, 0, "queue is empty\n"}.check)
	t.Run("T-MTA-34/q-interval", runModesCase{"", []string{"-q30m"}, 0, ""}.check)
	t.Run("version", runModesCase{"", []string{"--version"}, 0, "mailcrier "}.check)
	t.Run("help", runModesCase{"", []string{"--help"}, 0, "usage: mailcrier"}.check)
	t.Run("status-without-spool", runModesCase{"", []string{"--status"}, 0, "queued=0 held=0 failed=0 tmp=0 bytes=0 oldest_age_seconds=0\n"}.check)
}

type runModesCase struct {
	program    string
	args       []string
	want       int
	wantStdout string
}

func (tc runModesCase) check(t *testing.T) {
	t.Helper()
	rec := &recorder{}
	inv := &invocation{program: tc.program, config: twoTargets, args: tc.args, stdin: iotest.ErrReader(errors.New("stdin read")), deliver: rec.deliver}
	if code := inv.run(t); code != tc.want {
		t.Fatalf("Run() = %d, want %d; output:\n%s", code, tc.want, inv.output())
	}
	if got := inv.stdout.String(); !strings.HasPrefix(got, tc.wantStdout) || (tc.wantStdout == "") != (got == "") {
		t.Errorf("stdout = %q, want it to start with %q", got, tc.wantStdout)
	}
	if rec.calls != 0 {
		t.Error("message delivered")
	}
}

// TestRunWithoutRecipients pins the no-recipient policy: without routes
// every target is a catch-all, so a message without any recipient is
// delivered.
func TestRunWithoutRecipients(t *testing.T) {
	t.Run("T-MTA-18/no-t-no-arguments", runWithoutRecipientsCase{[]string{}, "Subject: t\n\nb\n"}.check)
	t.Run("T-MTA-19/t-without-recipient-headers", runWithoutRecipientsCase{[]string{"-t"}, "Subject: t\n\nb\n"}.check)
	t.Run("T-ADJ-08/t-without-headers-or-arguments", runWithoutRecipientsCase{[]string{"-t"}, "b\n"}.check)
	t.Run("T-CALL-11/mailx-t-without-recipients", runWithoutRecipientsCase{[]string{"-i", "-t"}, "Subject: t\nMIME-Version: 1.0\n\nb\n"}.check)
}

type runWithoutRecipientsCase struct {
	args  []string
	input string
}

func (tc runWithoutRecipientsCase) check(t *testing.T) {
	t.Helper()
	rec := &recorder{}
	inv := &invocation{config: twoTargets, args: tc.args, stdin: strings.NewReader(tc.input), deliver: rec.deliver}
	if code := inv.run(t); code != 0 {
		t.Fatalf("Run() = %d, want 0", code)
	}
	if len(rec.env.Recipients) != 0 || len(rec.data) != 2 {
		t.Errorf("recipients %q, %d deliveries; want none and 2", rec.env.Recipients, len(rec.data))
	}
	if !strings.Contains(inv.log("mailcrier"), "recipients=0") {
		t.Errorf("log lacks recipients=0:\n%s", inv.log("mailcrier"))
	}
}

// TestRunEmptyBody pins that input without visible text is delivered with
// a marker, because services reject an empty message.
func TestRunEmptyBody(t *testing.T) {
	t.Run("T-ADJ-10/empty-stdin", runEmptyBodyCase{[]string{"-t"}, ""}.check)
	t.Run("T-MTA-07/empty-stdin-with-argument-recipient", runEmptyBodyCase{[]string{"root"}, ""}.check)
	t.Run("T-CALL-21/headers-only", runEmptyBodyCase{[]string{"-i", "alice"}, "Subject: Output from your job 7\nTo: alice\n\n"}.check)
	t.Run("T-ADJ-36/whitespace-only", runEmptyBodyCase{[]string{"root"}, "   \n\t\n   "}.check)
	// Recipients and subject show that the header block ending at EOF was
	// read as headers, not dropped or taken as the body.
	headersOnly := func(input string) func(*testing.T) {
		return func(t *testing.T) {
			runEmptyBodyCase{[]string{"-t"}, input}.check(t)
			rec := &recorder{}
			inv := &invocation{config: twoTargets, args: []string{"-t"}, stdin: strings.NewReader(input), deliver: rec.deliver}
			if code := inv.run(t); code != 0 {
				t.Fatalf("Run() = %d, want 0", code)
			}
			if !slices.Equal(rec.env.Recipients, []string{"root"}) || len(rec.data) != 2 || rec.data[0].Subject != "zed event" {
				t.Errorf("recipients %q, data %+v; want root and the subject \"zed event\"", rec.env.Recipients, rec.data)
			}
		}
	}
	t.Run("T-CALL-22/headers-without-separator", headersOnly("To: root\nSubject: zed event\n"))
	t.Run("T-CALL-22/headers-without-final-newline", headersOnly("To: root\nSubject: zed event"))
}

type runEmptyBodyCase struct {
	args  []string
	input string
}

func (tc runEmptyBodyCase) check(t *testing.T) {
	t.Helper()
	rec := &recorder{}
	inv := &invocation{config: twoTargets, args: tc.args, stdin: strings.NewReader(tc.input), deliver: rec.deliver}
	if code := inv.run(t); code != 0 {
		t.Fatalf("Run() = %d, want 0", code)
	}
	if want := render.DefaultStrings().EmptyBody; len(rec.data) != 2 || !strings.HasSuffix(plainText(t, rec.data[0]), "\n"+want) {
		t.Errorf("deliveries = %+v, want 2 with the text ending %q", rec.data, want)
	}
}

// TestRunConfiguredNotice pins that [strings] replaces the notice for an
// empty body.
func TestRunConfiguredNotice(t *testing.T) {
	rec := &recorder{}
	inv := &invocation{config: "[strings]\nempty_body = \"(leer)\"\n\n" + twoTargets, args: []string{"root"}, stdin: strings.NewReader(""), deliver: rec.deliver}
	if code := inv.run(t); code != 0 {
		t.Fatalf("Run() = %d, want 0", code)
	}
	if len(rec.data) != 2 || !strings.HasSuffix(plainText(t, rec.data[0]), "\n(leer)") {
		t.Errorf("deliveries = %+v, want 2 with the text ending %q", rec.data, "(leer)")
	}
}

// TestRunDefaultSender pins the sender of a message without -f and From.
func TestRunDefaultSender(t *testing.T) {
	cases := []struct {
		name    string
		creds   Credentials
		environ []string
		want    string
	}{
		{"T-ADJ-06/email", plainUser, []string{"EMAIL=ops@example.org", "USER=bob"}, "ops@example.org"},
		{"T-ADJ-06/user", plainUser, []string{"USER=bob", "LOGNAME=carol"}, "bob@host1.example.org"},
		{"T-ADJ-06/logname", plainUser, []string{"LOGNAME=carol"}, "carol@host1.example.org"},
		{"T-ADJ-07/user-database", plainUser, nil, "alice@host1.example.org"},
		{"elevated-ignores-environment", elevatedUser, []string{"EMAIL=boss@example.org", "USER=root", "LOGNAME=root"}, "alice@host1.example.org"},
		{"unknown-uid", Credentials{UID: 4242, GID: 4242, EGID: 4242}, nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recorder{}
			inv := &invocation{config: twoTargets, args: []string{"root"}, creds: tc.creds, environ: tc.environ, stdin: strings.NewReader("Subject: t\n\nb\n"), deliver: rec.deliver}
			if code := inv.run(t); code != 0 {
				t.Fatalf("Run() = %d, want 0; output:\n%s", code, inv.output())
			}
			if rec.env.Sender != tc.want {
				t.Errorf("sender = %q, want %q", rec.env.Sender, tc.want)
			}
		})
	}
}

// TestRunMissingFrom pins where a message without From gets one: the
// template data and the hook environment take -f and -F, else the process
// user at the host name, while the message itself stays without From.
func TestRunMissingFrom(t *testing.T) {
	t.Run("T-MTA-22/f-and-full-name", func(t *testing.T) {
		rec := &recorder{}
		inv := &invocation{config: twoTargets, args: []string{"-f", "cron@example.org", "-F", "Cron Daemon", "root"}, stdin: strings.NewReader("Subject: t\n\nb\n"), deliver: rec.deliver}
		if code := inv.run(t); code != 0 {
			t.Fatalf("Run() = %d, want 0; output:\n%s", code, inv.output())
		}
		if len(rec.data) != 2 || rec.data[0].From.String() != "Cron Daemon <cron@example.org>" || rec.data[0].Headers.Has("From") {
			t.Errorf("data = %+v, want From \"Cron Daemon <cron@example.org>\" and no From header", rec.data)
		}
	})
	t.Run("T-MTA-22/process-user-and-host", func(t *testing.T) {
		rec := &recorder{}
		inv := &invocation{config: twoTargets, args: []string{"root"}, creds: plainUser, environ: []string{"USER=bob"}, stdin: strings.NewReader("Subject: t\n\nb\n"), deliver: rec.deliver}
		if code := inv.run(t); code != 0 {
			t.Fatalf("Run() = %d, want 0; output:\n%s", code, inv.output())
		}
		if len(rec.data) != 2 || rec.data[0].From.Addr != "bob@host1.example.org" || rec.data[0].Headers.Has("From") {
			t.Errorf("data = %+v, want From bob@host1.example.org and no From header", rec.data)
		}
	})
	t.Run("T-MTA-22/hook-env-from-message-unchanged", func(t *testing.T) {
		path := writeHookScript(t, "printf %s \"$MAILCRIER_FROM\" > \"$0.from\"\ncat > \"$0.stdin\"\n")
		const input = "Subject: t\n\nb\n"
		inv := &invocation{config: "[target.run]\ntype = \"exec\"\nargv = [\"" + path + "\"]\n", args: []string{"-f", "cron@example.org", "root"}, stdin: strings.NewReader(input)}
		if code := inv.run(t); code != 0 {
			t.Fatalf("Run() = %d, want 0; output:\n%s", code, inv.output())
		}
		from, err := os.ReadFile(path + ".from")
		if err != nil {
			t.Fatalf("hook did not run: %v; output:\n%s", err, inv.output())
		}
		stdin, err := os.ReadFile(path + ".stdin")
		if err != nil {
			t.Fatal(err)
		}
		if string(from) != "cron@example.org" || string(stdin) != input {
			t.Errorf("MAILCRIER_FROM = %q, stdin = %q; want cron@example.org and the message as given", from, stdin)
		}
	})
	t.Run("T-MTA-23/from-kept-f-sets-sender", func(t *testing.T) {
		rec := &recorder{}
		inv := &invocation{config: twoTargets, args: []string{"-f", "cron@example.org", "root"}, stdin: strings.NewReader("From: Bob <bob@example.com>\nSubject: t\n\nb\n"), deliver: rec.deliver}
		if code := inv.run(t); code != 0 {
			t.Fatalf("Run() = %d, want 0; output:\n%s", code, inv.output())
		}
		if rec.env.Sender != "cron@example.org" {
			t.Errorf("sender = %q, want cron@example.org", rec.env.Sender)
		}
		if len(rec.data) != 2 || rec.data[0].From.String() != "Bob <bob@example.com>" || rec.data[0].Headers.Get("From") != "Bob <bob@example.com>" {
			t.Errorf("data = %+v, want From and the From header \"Bob <bob@example.com>\"", rec.data)
		}
	})
}

// TestRunLogsOptionWarnings pins that an unknown flag is logged by name,
// without its value, and does not stop the delivery or add a recipient.
func TestRunLogsOptionWarnings(t *testing.T) {
	t.Run("T-MTA-28/unknown-flag-logged", func(t *testing.T) {
		rec := &recorder{}
		inv := &invocation{config: twoTargets, args: []string{"-x", "--color=secret-value", "-bv", "root"}, stdin: strings.NewReader("Subject: t\n\nb\n"), deliver: rec.deliver}
		if code := inv.run(t); code != 0 {
			t.Fatalf("Run() = %d, want 0", code)
		}
		log := inv.log("mailcrier")
		for _, want := range []string{
			`level=WARN msg="unknown option" option=-x`,
			`level=WARN msg="unknown option" option=--color`,
			`level=WARN msg="option ignored" option=-bv`,
		} {
			if !strings.Contains(log, want) {
				t.Errorf("log lacks %q:\n%s", want, log)
			}
		}
		if strings.Contains(log, "secret-value") {
			t.Errorf("log quotes the option value:\n%s", log)
		}
		if !slices.Equal(rec.env.Recipients, []string{"root"}) {
			t.Errorf("recipients = %q, want [root]", rec.env.Recipients)
		}
	})
	t.Run("warnings-suppressed", func(t *testing.T) {
		args := slices.Repeat([]string{"-z"}, 20)
		inv := &invocation{config: twoTargets, args: append(args, "root"), stdin: strings.NewReader("Subject: t\n\nb\n"), deliver: (&recorder{}).deliver}
		if code := inv.run(t); code != 0 {
			t.Fatalf("Run() = %d, want 0", code)
		}
		log := inv.log("mailcrier")
		if strings.Count(log, `msg="unknown option"`) != 16 || !strings.Contains(log, `level=WARN msg="warnings suppressed" count=4`) {
			t.Errorf("log does not cap the warnings:\n%s", log)
		}
	})
}

// TestRunLargeHeaderKeepsBcc pins that a Bcc field after more than 1 MiB
// of headers routes the message and stays out of the notification text.
func TestRunLargeHeaderKeepsBcc(t *testing.T) {
	t.Run("T-MTA-16/bcc-after-large-header", func(t *testing.T) {
		input := "Subject: s\nX-Filler: " + strings.Repeat("a", 1<<20) + "\nBcc: secret@example.org\n\nbody\n"
		rec := &recorder{}
		inv := &invocation{config: twoTargets, args: []string{"-t"}, stdin: strings.NewReader(input), deliver: rec.deliver}
		if code := inv.run(t); code != 0 {
			t.Fatalf("Run() = %d, want 0", code)
		}
		if !slices.Equal(rec.env.Recipients, []string{"secret@example.org"}) {
			t.Errorf("recipients = %q", rec.env.Recipients)
		}
		for _, d := range rec.data {
			if out := plainText(t, d); strings.Contains(out, "secret") || slices.Contains(d.Recipients, "secret@example.org") {
				t.Errorf("template data shows the Bcc address: %q, %q", out, d.Recipients)
			}
		}
	})
}

// TestRunReadWarnings pins that a malformed message is delivered as it
// came and the reader's warning reaches the log.
func TestRunReadWarnings(t *testing.T) {
	t.Run("T-ADJ-24/malformed-message-delivered", func(t *testing.T) {
		rec := &recorder{}
		inv := &invocation{config: twoTargets, args: []string{"root"}, stdin: strings.NewReader("Subject: t\nnot a header\nbody\n"), deliver: rec.deliver}
		if code := inv.run(t); code != 0 {
			t.Fatalf("Run() = %d, want 0", code)
		}
		if len(rec.data) != 2 || rec.data[0].Subject != "t" || rec.data[0].Body != "not a header\nbody\n" {
			t.Errorf("deliveries = %+v", rec.data)
		}
		if want := `level=WARN msg="` + message.WarningMalformedHeader + `"`; !strings.Contains(inv.log("mailcrier"), want) {
			t.Errorf("log lacks %q:\n%s", want, inv.log("mailcrier"))
		}
	})
	t.Run("T-ADJ-25/delivered-exits-0", func(t *testing.T) {
		inv := &invocation{config: twoTargets, stdin: strings.NewReader("Subject: t\n\nb\n"), deliver: (&recorder{}).deliver}
		if code := inv.run(t); code != 0 {
			t.Fatalf("Run() = %d, want 0", code)
		}
	})
	t.Run("T-ADJ-24/configuration-error", func(t *testing.T) {
		inv := &invocation{config: "colour = 1\n", stdin: strings.NewReader("Subject: t\n\nb\n")}
		if code := inv.run(t); code != 78 {
			t.Fatalf("Run() = %d, want 78", code)
		}
	})
	t.Run("T-ADJ-26/unreadable-input", func(t *testing.T) {
		inv := &invocation{config: twoTargets, stdin: iotest.ErrReader(errors.New("input/output error"))}
		if code := inv.run(t); code != 66 {
			t.Fatalf("Run() = %d, want 66", code)
		}
	})
}

// TestRunDotConvention pins fail2ban's call, which passes no -i: a line
// with a single dot ends the message. With -i the whole input arrives.
func TestRunDotConvention(t *testing.T) {
	input := "Subject: [Fail2Ban] sshd: banned 192.0.2.1\n\nwhois:\n.\nrest of whois\n"
	cases := []struct {
		name     string
		args     []string
		wantText string
	}{
		{"T-CALL-08/without-i", []string{"-f", "root@example.org", "root@example.com"}, "whois:\n"},
		{"T-CALL-08/with-i", []string{"-i", "-f", "root@example.org", "root@example.com"}, "whois:\n.\nrest of whois\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recorder{}
			inv := &invocation{config: twoTargets, args: tc.args, stdin: strings.NewReader(input), deliver: rec.deliver}
			if code := inv.run(t); code != 0 {
				t.Fatalf("Run() = %d, want 0", code)
			}
			if len(rec.data) == 0 || rec.data[0].Body != tc.wantText {
				t.Errorf("deliveries = %+v, want body %q", rec.data, tc.wantText)
			}
		})
	}
}

// TestRunCronieCommandLine runs cronie's mailer command line against a
// local HTTP receiver, as an operator would by hand:
//
//	printf 'To: root\nSubject: t\n\nb\n' | mailcrier -FCronDaemon -i -odi -oem -oi -t -f root
//
// The message arrives and the exit status is 0.
func TestRunCronieCommandLine(t *testing.T) {
	t.Run("T-MTA-44/cronie-to-http-receiver", func(t *testing.T) {
		bodies := make(chan map[string]string, 1)
		server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("receiver: %v", err)
			}
			bodies <- body
		}))
		defer server.Close()
		inv := &invocation{
			config: httpTargetConfig(server.URL),
			args:   []string{"-FCronDaemon", "-i", "-odi", "-oem", "-oi", "-t", "-f", "root"},
			stdin:  strings.NewReader("To: root\nSubject: t\n\nb\n"),
		}
		if code := inv.run(t); code != 0 {
			t.Fatalf("Run() = %d, want 0; output:\n%s", code, inv.output())
		}
		select {
		case body := <-bodies:
			if body["subject"] != "t" || body["body"] != "b\n" {
				t.Errorf("receiver got %v", body)
			}
		default:
			t.Fatal("receiver got no request")
		}
		if log := inv.log("mailcrier"); strings.Contains(log, "level=WARN") || !strings.Contains(log, "recipients=1") {
			t.Errorf("log has a warning or lacks recipients=1:\n%s", log)
		}
	})
}

// TestNotices pins that every configured notice replaces its built-in
// one and an absent one keeps it.
func TestNotices(t *testing.T) {
	got := notices(config.Strings{NoSubject: "a", EmptyBody: "b", Truncated: "c", TruncatedSize: "%s f", MoreAttachments: "%d d", NotSent: "e"})
	want := render.Strings{NoSubject: "a", EmptyBody: "b", Truncated: "c", TruncatedSize: "%s f", MoreAttachments: "%d d", NotSent: "e"}
	if got != want {
		t.Errorf("notices = %+v, want %+v", got, want)
	}
	if got := notices(config.Strings{}); got != render.DefaultStrings() {
		t.Errorf("notices of nothing = %+v", got)
	}
}

// TestRunKeepsConfiguredTruncated pins that a configured truncated
// without truncated_size ends a cut text that goes without its full
// text, in place of the English default with the size.
func TestRunKeepsConfiguredTruncated(t *testing.T) {
	bodies := make(chan map[string]string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("receiver: %v", err)
		}
		bodies <- body
	}))
	defer server.Close()
	inv := &invocation{
		config: httpTargetConfig(server.URL) + "max_text = 300\n[strings]\ntruncated = \"[обрезано]\"\n",
		stdin:  strings.NewReader("Subject: t\n\n" + strings.Repeat("строка\n", 100)),
	}
	if code := inv.run(t); code != 0 {
		t.Fatalf("Run() = %d, want 0; log:\n%s", code, inv.log("mailcrier"))
	}
	select {
	case body := <-bodies:
		if !strings.HasSuffix(body["body"], "строка\n[обрезано]") {
			t.Errorf("body = %q, want it to end with the configured notice", body["body"])
		}
	default:
		t.Fatal("receiver got no request")
	}
}
