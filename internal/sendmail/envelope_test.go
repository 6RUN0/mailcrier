package sendmail

import (
	"reflect"
	"strings"
	"testing"

	"github.com/6RUN0/mailcrier/internal/message"
)

// envelopeOf parses args, reads input as the message and returns the
// envelope with "fallback@host1.example.org" as the default sender.
func envelopeOf(t *testing.T, args []string, input string) (message.Envelope, *message.Message) {
	t.Helper()
	inv, _, err := Parse("sendmail", args)
	if err != nil {
		t.Fatal(err)
	}
	msg, blind, _, err := message.Read(strings.NewReader(input), message.ReadOptions{IgnoreDots: inv.IgnoreDots})
	if err != nil {
		t.Fatal(err)
	}
	return inv.Envelope(msg, blind, func() string { return "fallback@host1.example.org" }), msg
}

func TestEnvelope(t *testing.T) {
	t.Run("T-ADJ-01/positional-recipient", envelopeCase{
		args:  []string{"alice@example.com"},
		input: "From: bob@example.com\r\nTo: alice@example.com\r\nSubject: Test\r\n\r\nHello, world.\n",
		want:  message.Envelope{Sender: "bob@example.com", Recipients: []string{"alice@example.com"}},
	}.check)
	t.Run("T-ADJ-02/to-and-cc", envelopeCase{
		args:  []string{"-t"},
		input: "From: bob@example.com\nTo: alice@example.com\nCc: carol@example.com\n\nBody\n",
		want:  message.Envelope{Sender: "bob@example.com", Recipients: []string{"alice@example.com", "carol@example.com"}},
	}.check)
	t.Run("T-ADJ-03/bcc-becomes-recipient", envelopeCase{
		args:  []string{"-t"},
		input: "To: alice@example.com\nBcc: secret@example.com\n\nBody\n",
		want:  message.Envelope{Sender: "fallback@host1.example.org", Recipients: []string{"alice@example.com", "secret@example.com"}},
	}.check)
	t.Run("T-ADJ-04/f-overrides-from", envelopeCase{
		args:  []string{"-f", "sender@example.org", "alice@example.com"},
		input: "From: bob@example.com\n\nBody\n",
		want:  message.Envelope{Sender: "sender@example.org", Recipients: []string{"alice@example.com"}},
	}.check)
	t.Run("T-MTA-24/empty-sender-beats-from", envelopeCase{
		args:  []string{"-f", "<>", "root"},
		input: "From: bob@example.com\n\nBody\n",
		want:  message.Envelope{Sender: "", Recipients: []string{"root"}},
	}.check)
	t.Run("T-MTA-12/to-cc-bcc", envelopeCase{
		args:  []string{"-t"},
		input: "To: a@example.org, b@example.org\nCc: c@example.org\nBcc: d@example.org\nTo: e@example.org\n\nx\n",
		want:  message.Envelope{Sender: "fallback@host1.example.org", Recipients: []string{"a@example.org", "b@example.org", "e@example.org", "c@example.org", "d@example.org"}},
	}.check)
	t.Run("T-MTA-13/union-without-duplicates", envelopeCase{
		args:  []string{"-t", "a@example.org", "z@example.org"},
		input: "To: a@example.org\nCc: z@example.org, c@example.org\n\nx\n",
		want:  message.Envelope{Sender: "fallback@host1.example.org", Recipients: []string{"a@example.org", "z@example.org", "c@example.org"}},
	}.check)
	t.Run("T-ADJ-11/duplicates-collapse", envelopeCase{
		args:  []string{"alice@example.com", "alice@example.com", "bob@example.com"},
		input: "From: sender@example.com\nTo: alice@example.com\n\nBody\n",
		want:  message.Envelope{Sender: "sender@example.com", Recipients: []string{"alice@example.com", "bob@example.com"}},
	}.check)
	t.Run("headers-ignored-without-t", envelopeCase{
		args:  []string{"root"},
		input: "To: other@example.org\nBcc: hidden@example.org\n\nx\n",
		want:  message.Envelope{Sender: "fallback@host1.example.org", Recipients: []string{"root"}},
	}.check)
	t.Run("T-CALL-10/mailx-bcc", envelopeCase{
		args:  []string{"-i", "-t"},
		input: "To: ops@example.org\nSubject: report\nCc: dev@example.org\nBcc: audit@example.org,\n  archive@example.org\nMIME-Version: 1.0\n\nbody\n",
		want:  message.Envelope{Sender: "fallback@host1.example.org", Recipients: []string{"ops@example.org", "dev@example.org", "audit@example.org", "archive@example.org"}},
	}.check)
	t.Run("full-name", envelopeCase{
		args:  []string{"-FCronDaemon", "-froot", "-t"},
		input: "To: root\n\nx\n",
		want:  message.Envelope{Sender: "root", SenderName: "CronDaemon", Recipients: []string{"root"}},
	}.check)
	t.Run("T-MTA-14/resent-headers-replace-recipient-headers", envelopeCase{
		args:  []string{"-t"},
		input: "Resent-To: r@example.org\nResent-Cc: rc@example.org\nResent-Bcc: rb@example.org\nTo: a@example.org\nCc: c@example.org\nBcc: b@example.org\n\nx\n",
		want:  message.Envelope{Sender: "fallback@host1.example.org", Recipients: []string{"r@example.org", "rc@example.org", "rb@example.org"}},
	}.check)
	t.Run("T-MTA-14/resent-bcc-alone-replaces-to", envelopeCase{
		args:  []string{"-t"},
		input: "To: a@example.org\nResent-Bcc: rb@example.org\n\nx\n",
		want:  message.Envelope{Sender: "fallback@host1.example.org", Recipients: []string{"rb@example.org"}},
	}.check)
	t.Run("T-MTA-14/empty-resent-to-leaves-argv-only", envelopeCase{
		args:  []string{"-t", "root"},
		input: "To: a@example.org\nResent-To:\n\nx\n",
		want:  message.Envelope{Sender: "fallback@host1.example.org", Recipients: []string{"root"}},
	}.check)
	t.Run("T-MTA-14/resent-ignored-without-t", envelopeCase{
		args:  []string{"root"},
		input: "Resent-To: r@example.org\nResent-Bcc: rb@example.org\n\nx\n",
		want:  message.Envelope{Sender: "fallback@host1.example.org", Recipients: []string{"root"}},
	}.check)
	t.Run("T-ADJ-09/resent-from-and-resent-to", envelopeCase{
		args:  []string{"-t"},
		input: "From: original@example.com\r\nResent-From: forwarder@example.com\r\nResent-To: forwarded@example.com\r\nTo: first@example.com\r\nSubject: Fwd\r\n\r\nBody\n",
		want:  message.Envelope{Sender: "forwarder@example.com", Recipients: []string{"forwarded@example.com"}},
	}.check)
	t.Run("f-overrides-resent-from", envelopeCase{
		args:  []string{"-f", "sender@example.org", "root"},
		input: "From: original@example.com\nResent-From: forwarder@example.com\n\nBody\n",
		want:  message.Envelope{Sender: "sender@example.org", Recipients: []string{"root"}},
	}.check)
	t.Run("T-MTA-19/t-without-recipient-headers", envelopeCase{
		args:  []string{"-t"},
		input: "Subject: x\n\nx\n",
		want:  message.Envelope{Sender: "fallback@host1.example.org"},
	}.check)
}

type envelopeCase struct {
	args  []string
	input string
	want  message.Envelope
}

func (tc envelopeCase) check(t *testing.T) {
	t.Helper()
	got, msg := envelopeOf(t, tc.args, tc.input)
	if !reflect.DeepEqual(got, tc.want) {
		t.Errorf("Envelope() = %+v, want %+v", got, tc.want)
	}
	if msg.Header.Has("Bcc") || msg.Header.Has("Resent-Bcc") {
		t.Errorf("message keeps the Bcc or Resent-Bcc header: %v", msg.Header)
	}
}

// TestEnvelopeKeepsBccOutOfMessage pins that the Bcc addresses route the
// message but appear nowhere in the parsed message: not in its headers, its
// address lists or its body.
func TestEnvelopeKeepsBccOutOfMessage(t *testing.T) {
	t.Run("T-MTA-16/bcc-routes-but-is-hidden", func(t *testing.T) {
		env, msg := envelopeOf(t, []string{"-t"}, "To: a@example.org\nBcc: Hidden Person <hidden@example.org>\nSubject: s\n\nbody\n")
		if !strings.Contains(strings.Join(env.Recipients, " "), "hidden@example.org") {
			t.Errorf("recipients %q lack the Bcc address", env.Recipients)
		}
		dump := strings.Join([]string{msg.Subject, msg.Body}, "\n")
		for key, values := range msg.Header {
			dump += key + ": " + strings.Join(values, ",") + "\n"
		}
		for _, list := range [][]message.Address{msg.To, msg.Cc, {msg.From}} {
			for _, a := range list {
				dump += a.Name + " " + a.Addr + "\n"
			}
		}
		if strings.Contains(dump, "hidden") || strings.Contains(dump, "Hidden") {
			t.Errorf("message shows the Bcc address:\n%s", dump)
		}
	})
}
