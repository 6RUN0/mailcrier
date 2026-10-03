package render

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/6RUN0/mailcrier/internal/message"
	"github.com/6RUN0/mailcrier/internal/sendmail"
	"github.com/6RUN0/mailcrier/internal/text"
)

// readData reads input as sendmail -t -i would, with argv recipients, and
// returns the template data.
func readData(t *testing.T, input string, argvRecipients ...string) Data {
	t.Helper()
	inv, _, err := sendmail.Parse("sendmail", append([]string{"-t", "-i"}, argvRecipients...))
	if err != nil {
		t.Fatal(err)
	}
	msg, blind, _, err := message.Read(strings.NewReader(input), message.ReadOptions{IgnoreDots: true, MaxSize: message.MaxSize, ReceivedAt: testReceivedAt})
	if err != nil {
		t.Fatal(err)
	}
	env := inv.Envelope(msg, blind, func() string { return "root@host1.example.org" })
	env.Sender = "root@host1.example.org"
	d := NewData(msg, env, blind)
	d.Hostname = "host1.example.org"
	return d
}

// renderAll renders d with every built-in template.
func renderAll(t *testing.T, d Data) map[text.Format]string {
	t.Helper()
	out := map[text.Format]string{}
	for _, format := range builtinFormats {
		tmpl, err := Builtin(format)
		if err != nil {
			t.Fatal(err)
		}
		rendered, err := tmpl.Execute(d)
		if err != nil {
			t.Fatalf("%s: %v", format, err)
		}
		out[format] = rendered
	}
	return out
}

func TestNewDataHidesBcc(t *testing.T) {
	input := "From: cron@example.org\nTo: ops@example.org\nBcc: Hidden Person <hidden@example.org>, ops@example.org\nSubject: s\n\nbody\n"
	d := readData(t, input, "argv@example.org")
	t.Run("T-TPL-11/bcc-not-in-template-data", func(t *testing.T) {
		if want := []string{"argv@example.org", "ops@example.org"}; !slices.Equal(d.Recipients, want) {
			t.Errorf("Recipients = %q, want %q", d.Recipients, want)
		}
		dump := fmt.Sprintf("%+v", d)
		for _, out := range renderAll(t, d) {
			dump += out
		}
		custom, err := parse("custom", `{{ range .Headers.Names }}{{ . }}{{ end }}{{ .Headers.Get "Bcc" }}{{ .Recipients }}`, text.FormatPlain)
		if err != nil {
			t.Fatal(err)
		}
		out, err := custom.Execute(d)
		if err != nil {
			t.Fatal(err)
		}
		dump += out
		if strings.Contains(dump, "hidden") || strings.Contains(dump, "Hidden") || strings.Contains(dump, "Bcc") {
			t.Errorf("template data shows the Bcc header:\n%s", dump)
		}
	})
	t.Run("resent-bcc-not-in-template-data", func(t *testing.T) {
		d := readData(t, "From: cron@example.org\nTo: ops@example.org\nResent-To: fwd@example.org\nResent-Bcc: Secret Person <secret@example.org>\nSubject: s\n\nbody\n")
		if want := []string{"fwd@example.org"}; !slices.Equal(d.Recipients, want) {
			t.Errorf("Recipients = %q, want %q", d.Recipients, want)
		}
		custom, err := parse("custom", `{{ range .Headers.Names }}{{ . }}{{ end }}{{ .Headers.Get "Resent-Bcc" }}`, text.FormatPlain)
		if err != nil {
			t.Fatal(err)
		}
		out, err := custom.Execute(d)
		if err != nil {
			t.Fatal(err)
		}
		dump := fmt.Sprintf("%+v", d) + out
		for _, rendered := range renderAll(t, d) {
			dump += rendered
		}
		if strings.Contains(dump, "ecret") || strings.Contains(dump, "Bcc") {
			t.Errorf("template data shows the Resent-Bcc header:\n%s", dump)
		}
	})
}

// TestNewDataBoundsFrom pins the cut of an endless sender, which Fit does
// not shorten: the Telegram text still fits with the body.
func TestNewDataBoundsFrom(t *testing.T) {
	d := readData(t, "From: "+strings.Repeat("Cron Daemon ", 1000)+"<root@example.org>\nSubject: s\n\nbody\n")
	if n := utf8.RuneCountInString(d.From.Name); n > maxFromLength || n < maxFromLength-len("Cron Daemon ") || d.From.Addr != "root@example.org" {
		t.Fatalf("From = %d characters of name, address %q", n, d.From.Addr)
	}
	tmpl, err := Builtin(text.FormatTelegramHTML)
	if err != nil {
		t.Fatal(err)
	}
	if out, _, err := Fit(tmpl, d, 4096, text.MeasureTelegramHTML); err != nil || !strings.Contains(out, "body") {
		t.Errorf("Fit() = %q, err %v", out, err)
	}
}

func TestNewDataFromFallsBackToSender(t *testing.T) {
	d := readData(t, "Subject: s\n\nbody\n")
	if d.From != (message.Address{Addr: "root@host1.example.org"}) {
		t.Errorf("From = %+v", d.From)
	}
}

func TestBuiltinNotices(t *testing.T) {
	t.Run("T-TPL-02/empty-subject-and-body", func(t *testing.T) {
		checkNotices(t, "To: root\n\n")
	})
	t.Run("T-CALL-21/whitespace-only-body", func(t *testing.T) {
		checkNotices(t, "To: root\nSubject: \n\n \n\t\n")
	})
	t.Run("configured-notices", func(t *testing.T) {
		d := readData(t, "To: root\n\n")
		d.Strings = Strings{NoSubject: "(ohne Betreff)", EmptyBody: "(leer)"}
		out := renderAll(t, d)[text.FormatPlain]
		if !strings.Contains(out, "(ohne Betreff)") || !strings.Contains(out, "(leer)") {
			t.Errorf("plain = %q", out)
		}
	})
}

// TestBuiltinCollapsed pins the on_long blockquote rendering: the
// Telegram HTML template puts the body into an expandable blockquote in
// place of pre, the templates without such a block ignore the flag.
func TestBuiltinCollapsed(t *testing.T) {
	d := readData(t, "To: root\nSubject: s\n\nx < y\n")
	open := renderAll(t, d)
	d.IsCollapsed = true
	for format, out := range renderAll(t, d) {
		if format == text.FormatTelegramHTML {
			if !strings.Contains(out, "<blockquote expandable>x &lt; y</blockquote>") || strings.Contains(out, "<pre>") {
				t.Errorf("%s: %q", format, out)
			}
			continue
		}
		if out != open[format] {
			t.Errorf("%s: collapsed %q differs from %q", format, out, open[format])
		}
	}
}

func TestStringsWithFullSize(t *testing.T) {
	if got := DefaultStrings().WithFullSize(3 << 20).Truncated; got != "[truncated, 3.0 MiB in full]" {
		t.Errorf("Truncated = %q", got)
	}
	if got := (Strings{Truncated: "[cut]"}).WithFullSize(10).Truncated; got != "[cut]" {
		t.Errorf("without TruncatedSize: Truncated = %q", got)
	}
}

// TestBuiltinNotSent pins that every template listing attachments marks
// one the target does not send, escaped for its markup.
func TestBuiltinNotSent(t *testing.T) {
	d := readData(t, "To: root\nSubject: s\n\nbody\n")
	d.Attachments = []Attachment{{Name: "dump.tar", ContentType: "application/x-tar", Size: 60 << 20, IsSkipped: true}, {Name: "small.log", ContentType: "text/plain", Size: 10}}
	want := map[text.Format]string{
		text.FormatPlain: "[not sent]", text.FormatNtfy: "[not sent]", text.FormatDiscord: `\[not sent\]`,
		text.FormatSlackMrkdwn: "[not sent]", text.FormatTelegramHTML: "[not sent]", text.FormatTelegramMarkdownV2: `\[not sent\]`,
	}
	for format, out := range renderAll(t, d) {
		notice, isListed := want[format]
		if !isListed {
			continue
		}
		if strings.Count(out, notice) != 1 || !strings.Contains(out, "MiB) "+notice) && !strings.Contains(out, `MiB\) `+notice) {
			t.Errorf("%s: want %q once after the size of the skipped attachment:\n%s", format, notice, out)
		}
	}
}

func TestToJSON(t *testing.T) {
	t.Run("T-TPL-07/any-body-gives-valid-json", func(t *testing.T) {
		d := Data{Subject: "quote \" back \\ tab \t", Body: "nul \x00 esc \x1b bell \x07 invalid \xff\xfe end ", Hostname: "h"}
		tmpl, err := Builtin(text.FormatGenericJSON)
		if err != nil {
			t.Fatal(err)
		}
		out, err := tmpl.Execute(d)
		if err != nil {
			t.Fatal(err)
		}
		var decoded map[string]string
		if err := json.Unmarshal([]byte(out), &decoded); err != nil || !json.Valid([]byte(out)) {
			t.Fatalf("invalid JSON %s: %v", out, err)
		}
		if decoded["subject"] != d.Subject || !strings.HasPrefix(decoded["body"], "nul \x00 esc") {
			t.Errorf("decoded = %q", decoded)
		}
	})
}

// telegramTags are the tags the built-in Telegram HTML template writes.
var telegramTags = regexp.MustCompile(`</?(?:b|i|pre)>`)

func TestBuiltinMarkupStaysValid(t *testing.T) {
	input := "Content-Type: text/plain; charset=utf-8\nSubject: <b>bold</b> & *x* _y_ `z` <!channel>\n\n" +
		"bad \xc3\x28 \x00\x1b[31mred <script> & ``` @here . - _ * [a](b) ~ | { } ! # + = > \\\n"
	d := readData(t, input)
	outputs := renderAll(t, d)
	t.Run("T-CALL-27/invalid-utf8-and-controls", func(t *testing.T) {
		for format, out := range outputs {
			if !utf8.ValidString(out) {
				t.Errorf("%s: invalid UTF-8", format)
			}
			if jsonFormats[format] && !json.Valid([]byte(out)) {
				t.Errorf("%s: invalid JSON: %s", format, out)
			}
		}
		if rest := telegramTags.ReplaceAllString(outputs[text.FormatTelegramHTML], ""); strings.ContainsAny(rest, "<>") {
			t.Errorf("telegram-html leaves markup of the message: %s", outputs[text.FormatTelegramHTML])
		}
		if strings.Contains(outputs[text.FormatSlackMrkdwn], "<!channel>") {
			t.Errorf("slack-mrkdwn keeps a mention: %s", outputs[text.FormatSlackMrkdwn])
		}
	})
	t.Run("T-TPL-13/markdownv2-escapes-body", func(t *testing.T) {
		want := "*" + text.EscapeTelegramMarkdownV2(d.Subject) + "*\n_" + text.EscapeTelegramMarkdownV2(d.Hostname) + ": " +
			text.EscapeTelegramMarkdownV2(d.From.String()) + "_\n```\n" + text.EscapeTelegramMarkdownV2Code(strings.TrimRight(d.Body, "\n")) + "\n```"
		if got := outputs[text.FormatTelegramMarkdownV2]; got != want {
			t.Errorf("telegram-markdownv2 =\n%s\nwant\n%s", got, want)
		}
		if strings.Contains(outputs[text.FormatTelegramMarkdownV2], "host1.example") {
			t.Error("an unescaped dot reaches MarkdownV2 outside code")
		}
	})
	t.Run("code-blocks-stay-closed", func(t *testing.T) {
		for _, format := range []text.Format{text.FormatDiscord, text.FormatSlackMrkdwn, text.FormatSlackWebhook, text.FormatMattermost} {
			out := outputs[format]
			if jsonFormats[format] {
				var payload struct {
					Text string `json:"text"`
				}
				if err := json.Unmarshal([]byte(out), &payload); err != nil {
					t.Fatal(err)
				}
				out = payload.Text
			}
			if strings.Count(out, "```") != 2 {
				t.Errorf("%s has %d fences:\n%s", format, strings.Count(out, "```"), out)
			}
		}
	})
}

// checkNotices renders a message without subject and visible body with
// every built-in template and checks that both notices appear.
func checkNotices(t *testing.T, input string) {
	t.Helper()
	d := readData(t, input)
	for format, out := range renderAll(t, d) {
		if format == text.FormatGenericJSON {
			continue
		}
		if format != text.FormatNtfy && !strings.Contains(out, "no subject") {
			t.Errorf("%s lacks the subject notice:\n%s", format, out)
		}
		if !strings.Contains(out, "empty body") {
			t.Errorf("%s lacks the body notice:\n%s", format, out)
		}
	}
}

// mattermostMention matches a channel-wide mention as Mattermost does.
var mattermostMention = regexp.MustCompile(`(?i)(?:^|[^\w])@(?:channel|all|here)(?:[^\w]|$)`)

func TestMattermostMentions(t *testing.T) {
	t.Run("mattermost-channel-mentions-silenced", func(t *testing.T) {
		d := readData(t, "Subject: @channel disk full @ALL\n\n@here see log\n```\n@all again\n")
		var payload struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal([]byte(renderAll(t, d)[text.FormatMattermost]), &payload); err != nil {
			t.Fatal(err)
		}
		if mattermostMention.MatchString(payload.Text) {
			t.Errorf("mattermost text keeps a mention: %q", payload.Text)
		}
		if strings.Count(payload.Text, "```") != 2 {
			t.Errorf("mattermost text has %d fences: %q", strings.Count(payload.Text, "```"), payload.Text)
		}
	})
	t.Run("mattermost-markdown-escaped-outside-code", func(t *testing.T) {
		d := readData(t, "From: a_b*c <root@example.org>\nSubject: [click](https://example.com) *b* _i_ `c` @here\n\n[x](y) *z*\n")
		var payload struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal([]byte(renderAll(t, d)[text.FormatMattermost]), &payload); err != nil {
			t.Fatal(err)
		}
		want := "#### \\[click\\](https://example.com) \\*b\\* \\_i\\_ \\`c\\` @\u200bhere\n_host1.example.org: a\\_b\\*c \\<root@example.org\\>_\n```\n[x](y) *z*\n```"
		if payload.Text != want {
			t.Errorf("mattermost text =\n%s\nwant\n%s", payload.Text, want)
		}
	})
}

func TestHeadersInTemplates(t *testing.T) {
	d := readData(t, "Subject: s\nX-Cron-Env: <SHELL=/bin/sh>\nX-Cron-Env: <HOME=/root>\n\nbody\n")
	t.Run("T-CALL-20/cron-env-only-on-request", func(t *testing.T) {
		for format, out := range renderAll(t, d) {
			if strings.Contains(out, "SHELL") {
				t.Errorf("%s shows X-Cron-Env:\n%s", format, out)
			}
		}
		tmpl, err := parse("custom", `{{ .Headers.Get "x-cron-env" }}|{{ range .Headers.Values "X-Cron-Env" }}{{ . }};{{ end }}|{{ .Headers.Has "X-CRON-ENV" }}`, text.FormatPlain)
		if err != nil {
			t.Fatal(err)
		}
		out, err := tmpl.Execute(d)
		if err != nil {
			t.Fatal(err)
		}
		if want := "<SHELL=/bin/sh>|<SHELL=/bin/sh>;<HOME=/root>;|true"; out != want {
			t.Errorf("got %q, want %q", out, want)
		}
	})
}

func TestFunctions(t *testing.T) {
	for n, want := range map[int64]string{0: "0 B", 1023: "1023 B", 1536: "1.5 KiB", 20 << 20: "20.0 MiB", 3 << 30: "3.0 GiB"} {
		if got := humanizeBytes(n); got != want {
			t.Errorf("humanizeBytes(%d) = %q, want %q", n, got, want)
		}
	}
	for _, tc := range []struct {
		value, want any
	}{{"", "d"}, {" \n", "d"}, {"x", "x"}, {0, "d"}, {7, 7}, {nil, "d"}, {[]string{}, "d"}, {[]string{"a"}, []string{"a"}}} {
		if got := defaultValue("d", tc.value); fmt.Sprint(got) != fmt.Sprint(tc.want) {
			t.Errorf("defaultValue(%q) = %v, want %v", tc.value, got, tc.want)
		}
	}
	if _, err := Builtin("no-such-format"); err == nil {
		t.Error("Builtin accepts an unknown format")
	}
}
