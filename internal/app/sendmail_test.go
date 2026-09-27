package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/6RUN0/slendmail/internal/backend"
	"github.com/6RUN0/slendmail/internal/config"
	"github.com/6RUN0/slendmail/internal/delivery"
	"github.com/6RUN0/slendmail/internal/message"
	"github.com/6RUN0/slendmail/internal/render"
)

// recorder stands in for delivery: it keeps the envelope and payloads and
// answers each target with the status set for it, OK by default.
type recorder struct {
	statuses map[string]delivery.Status
	calls    int
	env      message.Envelope
	payloads []backend.Payload
}

func (r *recorder) deliver(_ context.Context, targets []delivery.Target, env message.Envelope, p backend.Payload) []delivery.Result {
	r.calls++
	r.env = env
	var results []delivery.Result
	for _, target := range targets {
		r.payloads = append(r.payloads, p)
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

// twoTargets configures targets "a" and "b"; the recorder never sends.
const twoTargets = "[target.a]\ntype = \"http\"\npreset = \"generic-json\"\nurl = \"http://127.0.0.1:1/a\"\n\n" +
	"[target.b]\ntype = \"http\"\npreset = \"generic-json\"\nurl = \"http://127.0.0.1:1/b\"\n"

// TestExitStatusValues pins the sysexits.h values the program uses, which
// are its own constants rather than an import.
func TestExitStatusValues(t *testing.T) {
	t.Run("T-MTA-35/sysexits-values", func(t *testing.T) {
		got := []int{exitOK, exitUsage, exitNoInput, exitSoftware, exitNoPerm, exitConfig}
		want := []int{0, 64, 66, 70, 77, 78}
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
				t.Fatalf("Run() = %d, want %d; log:\n%s", code, tc.want, inv.log("slendmail"))
			}
			log := inv.log("slendmail")
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
		deliver := func(context.Context, []delivery.Target, message.Envelope, backend.Payload) []delivery.Result {
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
	if got := inv.stderr.String(); got != "slendmail: "+tc.wantStderr+"\n" {
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
	t.Run("version", runModesCase{"", []string{"--version"}, 0, "slendmail "}.check)
	t.Run("help", runModesCase{"", []string{"--help"}, 0, "usage: slendmail"}.check)
	t.Run("status-not-implemented", runModesCase{"", []string{"--status"}, 64, ""}.check)
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
	if len(rec.env.Recipients) != 0 || len(rec.payloads) != 2 {
		t.Errorf("recipients %q, %d payloads; want none and 2", rec.env.Recipients, len(rec.payloads))
	}
	if !strings.Contains(inv.log("slendmail"), "recipients=0") {
		t.Errorf("log lacks recipients=0:\n%s", inv.log("slendmail"))
	}
}

// TestRunEmptyBody pins that input without visible text is delivered with
// a marker, because services reject an empty message.
func TestRunEmptyBody(t *testing.T) {
	t.Run("T-ADJ-10/empty-stdin", runEmptyBodyCase{[]string{"-t"}, ""}.check)
	t.Run("T-MTA-07/empty-stdin-with-argument-recipient", runEmptyBodyCase{[]string{"root"}, ""}.check)
	t.Run("T-CALL-21/headers-only", runEmptyBodyCase{[]string{"-i", "alice"}, "Subject: Output from your job 7\nTo: alice\n\n"}.check)
	t.Run("T-ADJ-36/whitespace-only", runEmptyBodyCase{[]string{"root"}, "   \n\t\n   "}.check)
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
	if want := render.DefaultStrings().EmptyBody; len(rec.payloads) != 2 || rec.payloads[0].Text != want {
		t.Errorf("payloads = %+v, want 2 with text %q", rec.payloads, want)
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
	if len(rec.payloads) != 2 || rec.payloads[0].Text != "(leer)" {
		t.Errorf("payloads = %+v, want 2 with text %q", rec.payloads, "(leer)")
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

// TestRunLogsOptionWarnings pins that an unknown flag is logged by name,
// without its value, and does not stop the delivery or add a recipient.
func TestRunLogsOptionWarnings(t *testing.T) {
	t.Run("T-MTA-28/unknown-flag-logged", func(t *testing.T) {
		rec := &recorder{}
		inv := &invocation{config: twoTargets, args: []string{"-x", "--color=secret-value", "-bv", "root"}, stdin: strings.NewReader("Subject: t\n\nb\n"), deliver: rec.deliver}
		if code := inv.run(t); code != 0 {
			t.Fatalf("Run() = %d, want 0", code)
		}
		log := inv.log("slendmail")
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
		log := inv.log("slendmail")
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
		for _, p := range rec.payloads {
			if strings.Contains(p.Title+p.Text, "secret") {
				t.Errorf("payload shows the Bcc address: %q", p.Text)
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
		if len(rec.payloads) != 2 || rec.payloads[0].Title != "t" || rec.payloads[0].Text != "not a header\nbody\n" {
			t.Errorf("payloads = %+v", rec.payloads)
		}
		if want := `level=WARN msg="` + message.WarningMalformedHeader + `"`; !strings.Contains(inv.log("slendmail"), want) {
			t.Errorf("log lacks %q:\n%s", want, inv.log("slendmail"))
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
			if len(rec.payloads) == 0 || rec.payloads[0].Text != tc.wantText {
				t.Errorf("payloads = %+v, want text %q", rec.payloads, tc.wantText)
			}
		})
	}
}

// TestRunCronieCommandLine runs cronie's mailer command line against a
// local HTTP receiver, as an operator would by hand:
//
//	printf 'To: root\nSubject: t\n\nb\n' | slendmail -FCronDaemon -i -odi -oem -oi -t -f root
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
		if log := inv.log("slendmail"); strings.Contains(log, "level=WARN") || !strings.Contains(log, "recipients=1") {
			t.Errorf("log has a warning or lacks recipients=1:\n%s", log)
		}
	})
}

// TestNotices pins that every configured notice replaces its built-in
// one and an absent one keeps it.
func TestNotices(t *testing.T) {
	got := notices(config.Strings{NoSubject: "a", EmptyBody: "b", Truncated: "c", MoreAttachments: "%d d", NotSent: "e"})
	if want := (render.Strings{NoSubject: "a", EmptyBody: "b", Truncated: "c", MoreAttachments: "%d d", NotSent: "e"}); got != want {
		t.Errorf("notices = %+v, want %+v", got, want)
	}
	if got := notices(config.Strings{}); got != render.DefaultStrings() {
		t.Errorf("notices of nothing = %+v", got)
	}
}
