package sendmail

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

// parseCase is one command line and the invocation it must produce.
type parseCase struct {
	argv0    string
	args     []string
	want     Invocation
	warnings []Warning
}

func (tc parseCase) check(t *testing.T) {
	t.Helper()
	argv0 := tc.argv0
	if argv0 == "" {
		argv0 = "/usr/sbin/sendmail"
	}
	got, warnings, err := Parse(argv0, tc.args)
	if err != nil {
		t.Fatalf("Parse(%q) error = %v", tc.args, err)
	}
	if !reflect.DeepEqual(got, tc.want) {
		t.Errorf("Parse(%q) =\n%+v\nwant\n%+v", tc.args, got, tc.want)
	}
	if !slices.Equal(warnings, tc.warnings) {
		t.Errorf("Parse(%q) warnings = %+v, want %+v", tc.args, warnings, tc.warnings)
	}
}

func TestParseCallers(t *testing.T) {
	t.Run("T-CALL-01/cronie", parseCase{
		args: strings.Fields("-FCronDaemon -i -odi -oem -oi -t -f root"),
		want: Invocation{Sender: "root", HasSender: true, FullName: "CronDaemon", ExtractRecipients: true, IgnoreDots: true},
	}.check)
	t.Run("T-CALL-02/debian-cron", parseCase{
		args: strings.Fields("-FCronDaemon -i -B8BITMIME -oem root"),
		want: Invocation{FullName: "CronDaemon", IgnoreDots: true, Recipients: []string{"root"}},
	}.check)
	t.Run("T-CALL-03/systemd-cron", parseCase{
		args: []string{"-i", "-B", "8BITMIME", "user@example.org"},
		want: Invocation{IgnoreDots: true, Recipients: []string{"user@example.org"}},
	}.check)
	t.Run("T-CALL-04/busybox-grouped", parseCase{
		args: []string{"-ti"},
		want: Invocation{ExtractRecipients: true, IgnoreDots: true},
	}.check)
	t.Run("T-CALL-05/mdadm-glued-sender", parseCase{
		args: []string{"-t", "-fraid@example.org"},
		want: Invocation{Sender: "raid@example.org", HasSender: true, ExtractRecipients: true},
	}.check)
	t.Run("T-CALL-06/oi-equals-i", parseCase{
		args: []string{"-oi", "-t"},
		want: Invocation{ExtractRecipients: true, IgnoreDots: true},
	}.check)
	t.Run("T-CALL-07/s-nail-double-dash", parseCase{
		args: []string{"-i", "--", "rcpt1@example.org", "-alias", "--probe"},
		want: Invocation{IgnoreDots: true, Recipients: []string{"rcpt1@example.org", "-alias", "--probe"}},
	}.check)
	t.Run("T-CALL-09/comma-in-arguments", parseCase{
		args: []string{"-i", "a@example.org,", "b@example.org", "c@example.org, d@example.org"},
		want: Invocation{IgnoreDots: true, Recipients: []string{"a@example.org", "b@example.org", "c@example.org", "d@example.org"}},
	}.check)
	t.Run("anacron", parseCase{
		args: []string{"-FAnacron", "-odi", "root"},
		want: Invocation{FullName: "Anacron", Recipients: []string{"root"}},
	}.check)
	t.Run("atd", parseCase{
		args: []string{"-i", "alice"},
		want: Invocation{IgnoreDots: true, Recipients: []string{"alice"}},
	}.check)
}

// TestParseSameModelFromShellAndExec pins that a command line split by a
// shell (mdadm through popen) and one passed as separate arguments (cronie
// through execvp) give the same invocation.
func TestParseSameModelFromShellAndExec(t *testing.T) {
	pairs := []struct{ glued, separate []string }{
		{[]string{"-t", "-froot@example.org"}, []string{"-t", "-f", "root@example.org"}},
		{[]string{"-FCronDaemon", "-oi"}, []string{"-F", "CronDaemon", "-o", "i"}},
		{[]string{"-B8BITMIME", "root"}, []string{"-B", "8BITMIME", "root"}},
	}
	for _, pair := range pairs {
		t.Run("T-CALL-31/"+strings.Join(pair.glued, ""), func(t *testing.T) {
			glued, _, err := Parse("sendmail", pair.glued)
			if err != nil {
				t.Fatal(err)
			}
			separate, _, err := Parse("sendmail", pair.separate)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(glued, separate) {
				t.Errorf("Parse(%q) = %+v, Parse(%q) = %+v", pair.glued, glued, pair.separate, separate)
			}
		})
	}
}

func TestParseFlags(t *testing.T) {
	t.Run("T-ADJ-05/msmtp-flag-set", parseCase{
		args: strings.Fields("-bm -G -m -n -U -v -B 8BITMIME -h 5 -L tag -N never -R full -V envid -A mode -p proto -O opt=val -o x val recipient@example.com"),
		// -o takes one value as in sendmail 8 and Postfix, so "val" is
		// an operand.
		want: Invocation{Recipients: []string{"val", "recipient@example.com"}},
	}.check)
	t.Run("T-ADJ-14/glued-values", parseCase{
		args: strings.Fields("-FCronDaemon -froot -oem -odi -B8BITMIME"),
		want: Invocation{Sender: "root", HasSender: true, FullName: "CronDaemon"},
	}.check)
	t.Run("T-MTA-24/f-separate", parseCase{
		args: []string{"-f", "root@example.org"},
		want: Invocation{Sender: "root@example.org", HasSender: true},
	}.check)
	t.Run("T-MTA-24/f-glued", parseCase{
		args: []string{"-froot@example.org"},
		want: Invocation{Sender: "root@example.org", HasSender: true},
	}.check)
	t.Run("T-MTA-24/r-separate", parseCase{
		args: []string{"-r", "root@example.org"},
		want: Invocation{Sender: "root@example.org", HasSender: true},
	}.check)
	t.Run("T-MTA-24/f-empty", parseCase{
		args: []string{"-f", ""},
		want: Invocation{HasSender: true},
	}.check)
	t.Run("T-MTA-24/f-null-sender", parseCase{
		args: []string{"-f", "<>"},
		want: Invocation{HasSender: true},
	}.check)
	t.Run("T-MTA-25/separate-values", parseCase{
		args: strings.Fields("-F CronDaemon -o em -o di -o i -B 8BITMIME -N never"),
		want: Invocation{FullName: "CronDaemon", IgnoreDots: true},
	}.check)
	t.Run("T-MTA-26/o-options-ignored", parseCase{
		args: strings.Fields("-oem -odi -om -o7 -o8 -O DeliveryMode=b -oA/etc/aliases"),
		want: Invocation{},
	}.check)
	t.Run("T-MTA-27/ignored-flags", parseCase{
		args:     strings.Fields("-B 8BITMIME -C /etc/mail/sendmail.cf -d0.1 -h 5 -L tag -m -n -N never -R hdrs -U -V envid -v -X /tmp/log -G -Aa root"),
		want:     Invocation{Recipients: []string{"root"}},
		warnings: []Warning{{Message: WarningIgnored, Option: "-C"}},
	}.check)
	t.Run("T-MTA-28/unknown-flag", parseCase{
		args:     []string{"-x", "-kti", "root"},
		want:     Invocation{ExtractRecipients: true, IgnoreDots: true, Recipients: []string{"root"}},
		warnings: []Warning{{Message: WarningUnknown, Option: "-x"}, {Message: WarningUnknown, Option: "-k"}},
	}.check)
	t.Run("T-MTA-28/unknown-long-option", parseCase{
		args:     []string{"--frobnicate=secret-value", "root"},
		want:     Invocation{Recipients: []string{"root"}},
		warnings: []Warning{{Message: WarningUnknown, Option: "--frobnicate"}},
	}.check)
	t.Run("T-MTA-29/double-dash", parseCase{
		args: []string{"-t", "--", "-oi", "-froot"},
		want: Invocation{ExtractRecipients: true, Recipients: []string{"-oi", "-froot"}},
	}.check)
	t.Run("T-ADJ-13/recipients-after-double-dash", parseCase{
		args: []string{"--", "a@example.org", "--", "-t"},
		want: Invocation{Recipients: []string{"a@example.org", "--", "-t"}},
	}.check)
	t.Run("T-MTA-30/bm-default", parseCase{
		args: []string{"-bm", "root"},
		want: Invocation{Mode: Deliver, Recipients: []string{"root"}},
	}.check)
	t.Run("operands-before-options", parseCase{
		args: []string{"root", "-t", "-i"},
		want: Invocation{ExtractRecipients: true, IgnoreDots: true, Recipients: []string{"root"}},
	}.check)
	t.Run("lone-dash-is-operand", parseCase{
		args: []string{"-"},
		want: Invocation{Recipients: []string{"-"}},
	}.check)
	t.Run("value-missing-at-end", parseCase{
		args:     []string{"root", "-B"},
		want:     Invocation{Recipients: []string{"root"}},
		warnings: []Warning{{Message: WarningMissingValue, Option: "-B"}},
	}.check)
}

func TestParseModes(t *testing.T) {
	t.Run("T-MTA-32/bi", parseCase{args: []string{"-bi"}, want: Invocation{Mode: NewAliases}}.check)
	t.Run("T-MTA-32/capital-i", parseCase{args: []string{"-I"}, want: Invocation{Mode: NewAliases}}.check)
	t.Run("T-MTA-32/newaliases-name", parseCase{argv0: "/usr/bin/newaliases", want: Invocation{Mode: NewAliases}}.check)
	t.Run("T-MTA-31/bp", parseCase{args: []string{"-bp"}, want: Invocation{Mode: ListQueue}}.check)
	t.Run("T-MTA-31/mailq-name", parseCase{argv0: "mailq", want: Invocation{Mode: ListQueue}}.check)
	t.Run("T-MTA-34/q", parseCase{args: []string{"-q"}, want: Invocation{Mode: RunQueue}}.check)
	t.Run("T-MTA-34/q-interval", parseCase{args: []string{"-q30m"}, want: Invocation{Mode: RunQueue}}.check)
	t.Run("q-leaves-next-argument", parseCase{args: []string{"-q", "root"}, want: Invocation{Mode: RunQueue, Recipients: []string{"root"}}}.check)
	t.Run("b-mode-first-letter-list", parseCase{args: []string{"-bpx"}, want: Invocation{Mode: ListQueue}}.check)
	t.Run("b-mode-first-letter-noop", parseCase{args: []string{"-bix"}, want: Invocation{Mode: NewAliases}}.check)
	t.Run("other-b-mode-ignored", parseCase{args: []string{"-bd"}, want: Invocation{}, warnings: []Warning{{Message: WarningIgnored, Option: "-bd"}}}.check)
	t.Run("last-mode-wins", parseCase{args: []string{"-bp", "-bm"}, want: Invocation{Mode: Deliver}}.check)
	t.Run("version", parseCase{args: []string{"--version"}, want: Invocation{Mode: Version}}.check)
	t.Run("help", parseCase{args: []string{"--help"}, want: Invocation{Mode: Help}}.check)
	t.Run("probe", parseCase{args: []string{"--probe"}, want: Invocation{Mode: Probe}}.check)
	t.Run("check-config", parseCase{args: []string{"--check-config"}, want: Invocation{Mode: CheckConfig}}.check)
	t.Run("status", parseCase{args: []string{"--status"}, want: Invocation{Mode: Status}}.check)
	t.Run("sendmail-name", parseCase{argv0: "sendmail", want: Invocation{}}.check)
	t.Run("mail-name-delivers", parseCase{argv0: "/usr/bin/mail", args: []string{"root"}, want: Invocation{Recipients: []string{"root"}}}.check)
}

// TestParseOptionValuesAreNotOptions pins that the value of a flag is
// never read as a long option: "-f --probe" names a sender, not a mode.
func TestParseOptionValuesAreNotOptions(t *testing.T) {
	t.Run("sender-looks-like-mode", parseCase{
		args: []string{"-f", "--probe", "root"},
		want: Invocation{Sender: "--probe", HasSender: true, Recipients: []string{"root"}},
	}.check)
	t.Run("full-name-looks-like-config", parseCase{
		args: []string{"-F", "--config=/tmp/evil.conf"},
		want: Invocation{FullName: "--config=/tmp/evil.conf"},
	}.check)
	t.Run("o-value-looks-like-option", parseCase{
		args: []string{"-o", "--check-config", "-B", "--version"},
		want: Invocation{},
	}.check)
	t.Run("config-value-is-not-parsed", parseCase{
		args: []string{"--config", "-t"},
		want: Invocation{ConfigPath: "-t"},
	}.check)
	t.Run("config-with-equals", parseCase{
		args: []string{"--config=/srv/app.conf", "-ti"},
		want: Invocation{ConfigPath: "/srv/app.conf", ExtractRecipients: true, IgnoreDots: true},
	}.check)
}

// TestParseEnvConfigMarker pins that the marker the re-exec adds is taken
// only as the first argument; anywhere else it is an unknown option before
// "--" and a recipient after it.
func TestParseEnvConfigMarker(t *testing.T) {
	t.Run("first", parseCase{
		args: []string{MarkerEnvConfig, "-ti"},
		want: Invocation{HasEnvConfigMarker: true, ExtractRecipients: true, IgnoreDots: true},
	}.check)
	t.Run("not-first", parseCase{
		args:     []string{"-t", MarkerEnvConfig},
		want:     Invocation{ExtractRecipients: true},
		warnings: []Warning{{Message: WarningUnknown, Option: MarkerEnvConfig}},
	}.check)
	t.Run("after-double-dash", parseCase{
		args: []string{"--", MarkerEnvConfig},
		want: Invocation{Recipients: []string{MarkerEnvConfig}},
	}.check)
}

func TestParseRejects(t *testing.T) {
	t.Run("T-ADJ-12/f-without-value", parseRejectsCase{[]string{"-t", "-f"}, "option -f requires a value"}.check)
	t.Run("r-without-value", parseRejectsCase{[]string{"-r"}, "option -r requires a value"}.check)
	t.Run("T-MTA-42/line-break-in-sender", parseRejectsCase{[]string{"-f", "root\nBcc: x@example.org"}, "line break in the sender address"}.check)
	t.Run("T-MTA-42/carriage-return-in-glued-sender", parseRejectsCase{[]string{"-froot\r"}, "line break in the sender address"}.check)
	t.Run("T-MTA-42/line-break-in-recipient", parseRejectsCase{[]string{"root\nx@example.org"}, "line break in a recipient address"}.check)
	t.Run("T-MTA-42/line-break-after-double-dash", parseRejectsCase{[]string{"--", "root\r\nx"}, "line break in a recipient address"}.check)
	t.Run("T-MTA-33/smtp-mode", parseRejectsCase{[]string{"-bs"}, "option -bs: the SMTP mode is not supported"}.check)
	t.Run("config-without-value", parseRejectsCase{[]string{"--config"}, "option --config requires a value"}.check)
	t.Run("config-empty", parseRejectsCase{[]string{"--config="}, "option --config requires a value"}.check)
	t.Run("T-MTA-33/smtp-mode-first-letter", parseRejectsCase{[]string{"-bsx"}, "option -bs: the SMTP mode is not supported"}.check)
	t.Run("line-break-in-full-name", parseRejectsCase{[]string{"-F", "Cron\nBcc: x@example.org"}, "line break in the full name"}.check)
	t.Run("carriage-return-in-glued-full-name", parseRejectsCase{[]string{"-FCron\r"}, "line break in the full name"}.check)
}

type parseRejectsCase struct {
	args []string
	want string
}

func (tc parseRejectsCase) check(t *testing.T) {
	t.Helper()
	_, _, err := Parse("sendmail", tc.args)
	if err == nil || err.Error() != tc.want {
		t.Fatalf("Parse(%q) error = %v, want %q", tc.args, err, tc.want)
	}
}

// FuzzParse checks that no command line panics Parse and that an accepted
// one never yields a recipient or sender with a line break.
func FuzzParse(f *testing.F) {
	for _, seed := range []string{
		"-FCronDaemon -i -odi -oem -oi -t -f root",
		"-ti", "-i -B 8BITMIME user@example.org", "-- -x", "--config /x -q30m",
		"-f", "-bs", "a@example.org,b@example.org", MarkerEnvConfig + " -t",
	} {
		f.Add("sendmail", seed)
	}
	f.Fuzz(func(t *testing.T, argv0, line string) {
		args := strings.Split(line, " ")
		inv, _, err := Parse(argv0, args)
		if err != nil {
			return
		}
		for _, value := range append([]string{inv.Sender, inv.FullName}, inv.Recipients...) {
			if strings.ContainsAny(value, "\r\n") {
				t.Errorf("Parse(%q) accepted a line break in %q", args, value)
			}
		}
		for _, r := range inv.Recipients {
			if r == "" || strings.Contains(r, ",") {
				t.Errorf("Parse(%q) recipient %q not split", args, r)
			}
		}
	})
}

// TestParseObsoleteValues pins sendmail 8's reading of a bare -d or -e:
// the next argument is the value only when it does not start with a dash,
// so "-e -t" keeps -t.
func TestParseObsoleteValues(t *testing.T) {
	t.Run("bare-e-before-flag", parseCase{args: []string{"-e", "-t"}, want: Invocation{ExtractRecipients: true}}.check)
	t.Run("bare-d-at-end", parseCase{args: []string{"root", "-d"}, want: Invocation{Recipients: []string{"root"}}}.check)
	t.Run("bare-d-with-value", parseCase{args: []string{"-d", "0.1", "root"}, want: Invocation{Recipients: []string{"root"}}}.check)
	t.Run("glued-e", parseCase{args: []string{"-em", "root"}, want: Invocation{Recipients: []string{"root"}}}.check)
}

// TestParseSenderBrackets pins that angle brackets around the value of -f
// or -r are removed, as sendmail 8 and Postfix do.
func TestParseSenderBrackets(t *testing.T) {
	t.Run("T-MTA-24/bracketed-sender", parseCase{args: []string{"-f", "<root@example.org>"}, want: Invocation{Sender: "root@example.org", HasSender: true}}.check)
	t.Run("T-MTA-24/glued-bracketed-sender", parseCase{args: []string{"-r<root>"}, want: Invocation{Sender: "root", HasSender: true}}.check)
	t.Run("unpaired-bracket-kept", parseCase{args: []string{"-f", "<root"}, want: Invocation{Sender: "<root", HasSender: true}}.check)
	t.Run("closing-bracket-kept", parseCase{args: []string{"-f", "root>"}, want: Invocation{Sender: "root>", HasSender: true}}.check)
}

// TestParseWarningLimits pins that a group of unknown letters yields one
// warning and that a command line yields at most maxWarnings warnings plus
// one that counts the rest.
func TestParseWarningLimits(t *testing.T) {
	t.Run("one-per-group", parseCase{
		args:     []string{"-" + strings.Repeat("z", 131071), "-zzti", "root"},
		want:     Invocation{ExtractRecipients: true, IgnoreDots: true, Recipients: []string{"root"}},
		warnings: []Warning{{Message: WarningUnknown, Option: "-z"}, {Message: WarningUnknown, Option: "-z"}},
	}.check)
	t.Run("capped", func(t *testing.T) {
		args := make([]string, 40)
		for i := range args {
			args[i] = "-z"
		}
		_, warnings, err := Parse("sendmail", args)
		if err != nil {
			t.Fatal(err)
		}
		want := Warning{Message: WarningSuppressed, Count: 40 - maxWarnings}
		if len(warnings) != maxWarnings+1 || warnings[maxWarnings] != want {
			t.Errorf("%d warnings, last %+v; want %d, last %+v", len(warnings), warnings[len(warnings)-1], maxWarnings+1, want)
		}
	})
}
