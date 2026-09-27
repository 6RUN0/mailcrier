package message

import (
	"encoding/base64"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// readMessage reads input with -i semantics and fails the test on error.
func readMessage(t *testing.T, input string) (*Message, []string) {
	t.Helper()
	msg, _, warnings, err := Read(strings.NewReader(input), ReadOptions{IgnoreDots: true, MaxSize: MaxSize})
	if err != nil {
		t.Fatal(err)
	}
	return msg, warnings
}

// TestReadDecodesBody pins the decoding of a single-part body: transfer
// encoding first, then the charset rule of decodeText.
func TestReadDecodesBody(t *testing.T) {
	t.Run("T-CALL-12/ascii-declared-utf8-body", bodyCase{"Content-Type: text/plain; charset=ANSI_X3.4-1968\nContent-Transfer-Encoding: 8bit\n\nФайловая система\n", "Файловая система\n"}.check)
	t.Run("T-CALL-12/us-ascii-declared-utf8-body", bodyCase{"Content-Type: text/plain; charset=us-ascii\n\nпривет\n", "привет\n"}.check)
	t.Run("ascii-declared-8bit-is-windows-1252", bodyCase{"Content-Type: text/plain; charset=us-ascii\n\na\xffb \x93q\x94\n", "a\u00ffb \u201cq\u201d\n"}.check)
	t.Run("latin1-declared-is-windows-1252", bodyCase{"Content-Type: text/plain; charset=iso-8859-1\n\n\x93caf\xe9\x94 \x96 \x80\n", "\u201ccaf\u00e9\u201d \u2013 \u20ac\n"}.check)
	t.Run("latin1-alias-is-windows-1252", bodyCase{"Content-Type: text/plain; charset=latin1\n\n\x85\n", "\u2026\n"}.check)
	t.Run("undeclared-8bit-is-windows-1252", bodyCase{"Subject: s\n\n\x93quoted\x94 \x81\n", "\u201cquoted\u201d \uFFFD\n"}.check)
	t.Run("replacement-charset-ascii-body", bodyCase{"Content-Type: text/plain; charset=iso-2022-kr\n\nhello world\n", "hello world\n"}.check)
	t.Run("replacement-charset-invalid-bytes", bodyCase{"Content-Type: text/plain; charset=csiso2022kr\n\na\xffb\n", "a\uFFFDb\n"}.check)
	t.Run("unknown-charset-utf8-body", bodyCase{"Content-Type: text/plain; charset=x-unknown\n\nпривет\n", "привет\n"}.check)
	t.Run("unknown-charset-invalid-bytes", bodyCase{"Content-Type: text/plain; charset=x-unknown\n\na\xffb\n", "a\uFFFDb\n"}.check)
	t.Run("T-CALL-13/quoted-printable-soft-breaks", bodyCase{"Content-Type: text/plain; charset=utf-8\nContent-Transfer-Encoding: quoted-printable\n\n=C3=BCberpr=C3=BCft, eine sehr lange Zeile die =\nweich umgebrochen wird\n", "überprüft, eine sehr lange Zeile die weich umgebrochen wird\n"}.check)
	t.Run("quoted-printable-invalid-escape-kept", bodyCase{"Content-Transfer-Encoding: quoted-printable\n\n100=% sure =C3=A9\n", "100=% sure é\n"}.check)
	t.Run("T-CALL-16/no-mime-utf8", bodyCase{"To: root\nSubject: s\n\nüber\n", "über\n"}.check)
	t.Run("T-CALL-16/no-mime-latin1", bodyCase{"To: root\nSubject: s\n\n\xfcber Soci\xe9t\xe9\n", "über Société\n"}.check)
	t.Run("T-CALL-17/content-type-without-mime-version", bodyCase{"Content-Type: text/plain; charset=\"ISO-8859-1\"\nContent-Transfer-Encoding: quoted-printable\n\nSoci=E9t=E9\n", "Société\n"}.check)
	t.Run("T-CALL-18/binary-and-quoted-charset", bodyCase{"Content-Type: text/plain; charset=\"utf-8\"\nMIME-Version: 1.0\nContent-Transfer-Encoding: binary\n\nstatus: ok \u2713\n", "status: ok \u2713\n"}.check)
	t.Run("T-TPL-14/latin1-base64-body", bodyCase{"MIME-Version: 1.0\nContent-Type: text/plain; charset=ISO-8859-1\nContent-Transfer-Encoding: base64\n\n" + base64.StdEncoding.EncodeToString([]byte("Anna-V\xe9ronique\r\n")) + "\n", "Anna-Véronique\n"}.check)
	t.Run("koi8-r-body", bodyCase{"Content-Type: text/plain; charset=koi8-r\n\n\xf0\xd2\xc9\xd7\xc5\xd4\n", "Привет\n"}.check)
	t.Run("windows-1251-body", bodyCase{"Content-Type: text/plain; charset=windows-1251\n\n\xcf\xf0\xe8\xe2\xe5\xf2\n", "Привет\n"}.check)
	t.Run("base64-with-garbage", bodyCase{"Content-Transfer-Encoding: base64\n\naGVs\n bG8=\n!!\n", "hello"}.check)
	t.Run("base64-crlf-text", bodyCase{"Content-Transfer-Encoding: base64\n\n" + base64.StdEncoding.EncodeToString([]byte("a\r\nb\r\n")) + "\n", "a\nb\n"}.check)
	t.Run("unreadable-content-type", bodyCase{"Content-Type: ;;;\n\nbody\n", "body\n"}.check)
	t.Run("T-CALL-27/invalid-utf8-and-controls", bodyCase{"Content-Type: text/plain; charset=utf-8\n\nbad \xc3\x28 \x00\x1b[31mred\n", "bad \uFFFD( \x00\x1b[31mred\n"}.check)
}

// TestReadDecodesHeaders pins encoded words, raw 8-bit values and the
// white space of the Subject.
func TestReadDecodesHeaders(t *testing.T) {
	t.Run("T-ADJ-52/q-encoded", subjectCase{"Subject: =?utf-8?q?h=C3=B4st_down?=\n\nb\n", "hôst down"}.check)
	t.Run("T-ADJ-52/b-encoded", subjectCase{"Subject: =?UTF-8?B?0J/RgNC40LLQtdGC?=\n\nb\n", "Привет"}.check)
	t.Run("T-ADJ-52/lowercase-name", subjectCase{"subject: lower\n\nb\n", "lower"}.check)
	t.Run("T-ADJ-52/uppercase-name", subjectCase{"SUBJECT: upper\n\nb\n", "upper"}.check)
	t.Run("T-CALL-14/q-encoded-over-two-words", subjectCase{"Subject: =?utf-8?q?=5Breboot_required=5D_Ergebnis_von_unattended-upgrades_f?=\n =?utf-8?q?=C3=BCr_host1=3A_ERFOLG_mit_einer_sehr_langen_Zeile?=\n\nb\n", "[reboot required] Ergebnis von unattended-upgrades für host1: ERFOLG mit einer sehr langen Zeile"}.check)
	t.Run("T-CALL-15/raw-utf8", subjectCase{"Subject: Cron <root@host1> echo привет\n\nb\n", "Cron <root@host1> echo привет"}.check)
	t.Run("raw-latin1", subjectCase{"Subject: caf\xe9\n\nb\n", "café"}.check)
	t.Run("raw-8bit-is-windows-1252", subjectCase{"Subject: \x93caf\xe9\x94 \x96 ok\n\nb\n", "\u201ccaf\u00e9\u201d \u2013 ok"}.check)
	t.Run("us-ascii-word-follows-body-rule", subjectCase{"Subject: =?us-ascii?Q?=D0=BF=D1=80?= =?US-ASCII?Q?=93q=94?=\n\nb\n", "\u043f\u0440\u201cq\u201d"}.check)
	t.Run("iso-8859-1-word-is-windows-1252", subjectCase{"Subject: =?iso-8859-1?Q?=93caf=E9=94?=\n\nb\n", "\u201ccaf\u00e9\u201d"}.check)
	t.Run("replacement-charset-word", subjectCase{"Subject: =?iso-2022-kr?Q?hello?=\n\nb\n", "hello"}.check)
	t.Run("T-TPL-14/latin1-q-encoded", subjectCase{"Subject: =?ISO-8859-1?Q?Anna-V=E9ronique?=\n\nb\n", "Anna-Véronique"}.check)
	t.Run("koi8-r-encoded-word", subjectCase{"Subject: =?koi8-r?B?8NLJ18XU?=\n\nb\n", "Привет"}.check)
	t.Run("text-between-words-kept", subjectCase{"Subject: =?utf-8?q?a?= and =?utf-8?q?b?=\n\nb\n", "a and b"}.check)
	t.Run("malformed-word-kept", subjectCase{"Subject: =?utf-8?x?abc?= =?utf-8?B?!!?= x\n\nb\n", "=?utf-8?x?abc?= =?utf-8?B?!!?= x"}.check)
	t.Run("word-inside-malformed-one", subjectCase{"Subject: =?a?=?utf-8?q?ok?=\n\nb\n", "=?a?ok"}.check)
	t.Run("T-ADJ-37/folded-with-repeated-spaces", subjectCase{"Subject:   first   part\n   second\t\tpart  \n\nb\n", "first part second part"}.check)
	t.Run("T-CALL-20/cron-env-headers", func(t *testing.T) {
		msg, _ := readMessage(t, "Subject: s\nX-Cron-Env: <SHELL=/bin/sh>\nX-Cron-Env: <HOME=/root>\n\nbody\n")
		if got := msg.Header.Get("x-cron-env"); got != "<SHELL=/bin/sh>" {
			t.Errorf("Get = %q", got)
		}
		if got := msg.Header.Values("X-CRON-ENV"); !slices.Equal(got, []string{"<SHELL=/bin/sh>", "<HOME=/root>"}) {
			t.Errorf("Values = %q", got)
		}
		if !msg.Header.Has("x-cron-env") || msg.Header.Has("Bcc") {
			t.Error("Has is wrong")
		}
		if got := msg.Header.Names(); !slices.Equal(got, []string{"Subject", "X-Cron-Env"}) {
			t.Errorf("Names = %q", got)
		}
		if strings.Contains(msg.Body, "SHELL") {
			t.Errorf("Body carries the headers: %q", msg.Body)
		}
	})
}

// TestReadDate pins that a missing or unreadable Date is the receive time.
func TestReadDate(t *testing.T) {
	received := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	cases := []struct {
		name  string
		input string
		want  time.Time
	}{
		{"T-MTA-20/kept", "Date: Sat, 27 Sep 2026 08:30:00 +0200\n\nb\n", time.Date(2026, 9, 27, 6, 30, 0, 0, time.UTC)},
		{"T-MTA-20/missing", "Subject: s\n\nb\n", received},
		{"unreadable", "Date: yesterday\n\nb\n", received},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg, _, _, err := Read(strings.NewReader(tc.input), ReadOptions{ReceivedAt: received})
			if err != nil {
				t.Fatal(err)
			}
			if !msg.Date.Equal(tc.want) {
				t.Errorf("Date = %v, want %v", msg.Date, tc.want)
			}
		})
	}
}

// TestReadAddressLists pins the address forms of RFC 5322 in To and Cc.
func TestReadAddressLists(t *testing.T) {
	input := "To: Team: a@example.org, \"Doe, Jane\" <jane@example.org>;, undisclosed:;\n" +
		"To: =?utf-8?q?J=C3=BCrgen?= <juergen@example.org>,\n (comment) b@example.org\n" +
		"Cc: =?koi8-r?B?8NLJ18XU?= <root>, \"Quoted \\\"Name\\\"\" <q@example.com>\n\nbody\n"
	msg, _ := readMessage(t, input)
	wantTo := []Address{
		{Addr: "a@example.org"}, {Name: "Doe, Jane", Addr: "jane@example.org"},
		{Name: "Jürgen", Addr: "juergen@example.org"}, {Name: "comment", Addr: "b@example.org"},
	}
	t.Run("T-MTA-17/groups-comments-encoded-names-repeated-to", func(t *testing.T) {
		if !reflect.DeepEqual(msg.To, wantTo) {
			t.Errorf("To = %+v, want %+v", msg.To, wantTo)
		}
		wantCc := []Address{{Name: "Привет", Addr: "root"}, {Name: "Quoted \"Name\"", Addr: "q@example.com"}}
		if !reflect.DeepEqual(msg.Cc, wantCc) {
			t.Errorf("Cc = %+v, want %+v", msg.Cc, wantCc)
		}
	})
	t.Run("us-ascii-encoded-name-follows-body-rule", func(t *testing.T) {
		msg, _ := readMessage(t, "To: =?us-ascii?Q?=D0=BF?= <a@example.org>, =?ISO-8859-1?Q?=93x=94?= <b@example.org>\n\nbody\n")
		want := []Address{{Name: "\u043f", Addr: "a@example.org"}, {Name: "\u201cx\u201d", Addr: "b@example.org"}}
		if !reflect.DeepEqual(msg.To, want) {
			t.Errorf("To = %+v, want %+v", msg.To, want)
		}
	})
	for _, tc := range []struct {
		name, value string
		want        []Address
	}{
		{"encoded-word-address-kept", "=?us-ascii?q?x?=@example.org", []Address{{Addr: "=?us-ascii?q?x?=@example.org"}}},
		{"quoted-encoded-word-name-kept", `"=?us-ascii?q?x?=" <a@example.org>`, []Address{{Name: "=?us-ascii?q?x?=", Addr: "a@example.org"}}},
		{"encoded-name-beside-encoded-address", "=?us-ascii?Q?=D0=BF?= <=?iso-8859-1?q?y?=@example.org>", []Address{{Name: "\u043f", Addr: "=?iso-8859-1?q?y?=@example.org"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ParseAddressList(tc.value); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ParseAddressList(%q) = %+v, want %+v", tc.value, got, tc.want)
			}
		})
	}
	if got := (Address{Name: "Cron Daemon", Addr: "root"}).String(); got != "Cron Daemon <root>" {
		t.Errorf("String() = %q", got)
	}
}

// TestReadMultipart pins how parts become the body, the HTML body and
// attachments.
func TestReadMultipart(t *testing.T) {
	t.Run("T-ADJ-38/mixed-with-attachment", func(t *testing.T) {
		input := "MIME-Version: 1.0\nContent-Type: multipart/mixed; boundary=\"b1\"\n\npreamble\n--b1\n" +
			"Content-Type: text/plain; charset=utf-8\nContent-Transfer-Encoding: quoted-printable\n\nReport =C3=A9t=C3=A9\n" +
			"--b1\nContent-Type: application/pdf; name=\"=?utf-8?q?r=C3=A9sum=C3=A9.pdf?=\"\nContent-Disposition: attachment\nContent-Transfer-Encoding: base64\n\n" +
			base64.StdEncoding.EncodeToString([]byte("%PDF-1.4")) + "\n--b1--\nepilogue\n"
		msg, warnings := readMessage(t, input)
		if msg.Body != "Report été" {
			t.Errorf("Body = %q", msg.Body)
		}
		want := []Attachment{{Name: "résumé.pdf", ContentType: "application/pdf", Size: 8, Data: []byte("%PDF-1.4")}}
		if !reflect.DeepEqual(msg.Attachments, want) {
			t.Errorf("Attachments = %+v, want %+v", msg.Attachments, want)
		}
		if len(warnings) != 0 {
			t.Errorf("warnings = %q", warnings)
		}
	})
	t.Run("T-ADJ-39/only-attachment", func(t *testing.T) {
		input := "Content-Type: multipart/mixed; boundary=b\n\n--b\nContent-Type: text/csv\nContent-Disposition: attachment; filename=\"../../etc/report.csv\"\n\na,b\n--b--\n"
		msg, warnings := readMessage(t, input)
		if msg.Body != "" {
			t.Errorf("Body = %q, want empty", msg.Body)
		}
		if len(msg.Attachments) != 1 || msg.Attachments[0].Name != "report.csv" || string(msg.Attachments[0].Data) != "a,b" {
			t.Errorf("Attachments = %+v", msg.Attachments)
		}
		if len(warnings) != 0 {
			t.Errorf("warnings = %q", warnings)
		}
	})
	t.Run("alternative-prefers-plain-keeps-html", func(t *testing.T) {
		input := "Content-Type: multipart/alternative; boundary=alt\n\n--alt\nContent-Type: text/plain\n\nplain text\n--alt\nContent-Type: text/html\n\n<p>html <b>text</b></p>\n--alt--\n"
		msg, _ := readMessage(t, input)
		if msg.Body != "plain text" || msg.BodyHTML != "<p>html <b>text</b></p>" {
			t.Errorf("Body = %q, BodyHTML = %q", msg.Body, msg.BodyHTML)
		}
	})
	t.Run("nested-related-html-only", func(t *testing.T) {
		input := "Content-Type: multipart/mixed; boundary=m\n\n--m\nContent-Type: multipart/alternative; boundary=a\n\n--a\nContent-Type: multipart/related; boundary=r\n\n--r\nContent-Type: text/html; charset=iso-8859-1\n\n<p>caf\xe9</p>\n--r\nContent-Type: image/png\nContent-ID: <i1>\n\nPNG\n--r--\n--a--\n--m--\n"
		msg, _ := readMessage(t, input)
		if msg.Body != "café\n" || msg.BodyHTML != "<p>café</p>" {
			t.Errorf("Body = %q, BodyHTML = %q", msg.Body, msg.BodyHTML)
		}
		if len(msg.Attachments) != 1 || msg.Attachments[0].ContentType != "image/png" {
			t.Errorf("Attachments = %+v", msg.Attachments)
		}
	})
	t.Run("alternative-with-related-html", func(t *testing.T) {
		input := "Content-Type: multipart/alternative; boundary=a\n\n--a\nContent-Type: text/plain\n\nplain text\n--a\n" +
			"Content-Type: multipart/related; boundary=r\n\n--r\nContent-Type: text/html\n\n<p>html</p>\n--r\nContent-Type: image/png\nContent-ID: <i1>\n\nPNG\n--r--\n--a--\n"
		msg, _ := readMessage(t, input)
		if msg.Body != "plain text" || msg.BodyHTML != "<p>html</p>" {
			t.Errorf("Body = %q, BodyHTML = %q", msg.Body, msg.BodyHTML)
		}
		if len(msg.Attachments) != 1 || msg.Attachments[0].ContentType != "image/png" || string(msg.Attachments[0].Data) != "PNG" {
			t.Errorf("Attachments = %+v", msg.Attachments)
		}
	})
	t.Run("named-inline-text-is-body", func(t *testing.T) {
		input := "Content-Type: multipart/mixed; boundary=b\n\n--b\nContent-Type: text/plain\n\nintro\n--b\n" +
			"Content-Type: text/plain; name=\"run.log\"\nContent-Disposition: inline; filename=\"run.log\"\n\nlog line\n--b\n" +
			"Content-Type: text/plain; name=\"data.txt\"\nContent-Disposition: attachment\n\ndata\n--b--\n"
		msg, _ := readMessage(t, input)
		if msg.Body != "intro\nlog line" {
			t.Errorf("Body = %q", msg.Body)
		}
		if len(msg.Attachments) != 1 || msg.Attachments[0].Name != "data.txt" {
			t.Errorf("Attachments = %+v", msg.Attachments)
		}
	})
	t.Run("named-single-part-text-is-body", func(t *testing.T) {
		msg, _ := readMessage(t, "Content-Type: text/plain; name=\"log.txt\"\n\nlog line\n")
		if msg.Body != "log line\n" || len(msg.Attachments) != 0 {
			t.Errorf("Body = %q, Attachments = %+v", msg.Body, msg.Attachments)
		}
	})
	t.Run("two-text-parts-joined", func(t *testing.T) {
		msg, _ := readMessage(t, "Content-Type: multipart/mixed; boundary=b\n\n--b\n\nfirst\n--b\n\nsecond\n--b--\n")
		if msg.Body != "first\nsecond" {
			t.Errorf("Body = %q", msg.Body)
		}
	})
	t.Run("missing-close-delimiter", func(t *testing.T) {
		msg, warnings := readMessage(t, "Content-Type: multipart/mixed; boundary=b\n\n--b\n\nonly part\n")
		if msg.Body != "only part\n" || len(warnings) != 0 {
			t.Errorf("Body = %q, warnings = %q", msg.Body, warnings)
		}
	})
	t.Run("boundary-prefix-is-not-a-delimiter", func(t *testing.T) {
		msg, _ := readMessage(t, "Content-Type: multipart/mixed; boundary=b\n\n--b\n\nline\n--bb not a delimiter\n--b--\n")
		if msg.Body != "line\n--bb not a delimiter" {
			t.Errorf("Body = %q", msg.Body)
		}
	})
	t.Run("boundary-never-found", func(t *testing.T) {
		msg, warnings := readMessage(t, "Content-Type: multipart/mixed; boundary=zz\n\njust text\n")
		if msg.Body != "just text\n" || !slices.Equal(warnings, []string{WarningMalformedMIME}) {
			t.Errorf("Body = %q, warnings = %q", msg.Body, warnings)
		}
	})
	t.Run("too-deep-is-attachment", func(t *testing.T) {
		var input strings.Builder
		input.WriteString("Subject: s\n")
		for i := range maxMultipartDepth + 1 {
			input.WriteString("Content-Type: multipart/mixed; boundary=b" + string(rune('a'+i)) + "\n\n--b" + string(rune('a'+i)) + "\n")
		}
		input.WriteString("Content-Type: text/plain\n\ndeep\n")
		msg, _ := readMessage(t, input.String())
		if msg.Body != "" || len(msg.Attachments) != 1 || msg.Attachments[0].ContentType != "multipart/mixed" {
			t.Errorf("Body = %q, Attachments = %+v", msg.Body, msg.Attachments)
		}
	})
}

// TestAttachmentName pins that a file name loses directories, control
// characters and bidirectional controls, and that "." and ".." become no
// name.
func TestAttachmentName(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"plain", "report.pdf", "report.pdf"},
		{"directories", `C:\x\..\report.csv`, "report.csv"},
		{"dot-dot", "..", ""},
		{"dot", "dir/.", ""},
		{"dot-dot-after-directory", `C:\x\..`, ""},
		{"right-to-left-override", "evil\u202egpj.exe", "evilgpj.exe"},
		{"bidi-marks-and-isolates", "a\u200eb\u200fc\u202ad\u2066e\u2069f", "abcdef"},
		{"c0-and-c1-controls", "a\x00b\x1bc\u0085d\u009fe\x7f", "abcde"},
		{"encoded-word", "=?utf-8?q?r=C3=A9sum=C3=A9.pdf?=", "résumé.pdf"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := attachmentName(tc.in); got != tc.want {
				t.Errorf("attachmentName(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestReadHTMLBody pins that a text/html body is converted to text.
func TestReadHTMLBody(t *testing.T) {
	input := "Content-Type: text/html; charset=\"utf-8\"\n\n<html><head><title>apt-listchanges output</title></head><body>\n<pre>example-tool (2.0-1)\n  * Fix <a href=\"https://example.org/bugs/1\">bug 1</a>.</pre>\n</body></html>\n"
	t.Run("T-CALL-19/html-keeps-links", func(t *testing.T) {
		msg, _ := readMessage(t, input)
		want := "example-tool (2.0-1)\n  * Fix bug 1 (https://example.org/bugs/1).\n"
		if msg.Body != want {
			t.Errorf("Body = %q, want %q", msg.Body, want)
		}
		if !strings.Contains(msg.BodyHTML, "<pre>") {
			t.Errorf("BodyHTML = %q", msg.BodyHTML)
		}
	})
}

func TestHTMLToText(t *testing.T) {
	cases := []struct {
		name string
		html string
		want string
	}{
		{"T-ADJ-61/dropped-elements", "a<script>alert(1)</script>b<style>p{}</style>c<iframe src=x>i</iframe>d<object>o</object>e<embed src=x>f<svg><svg>s</svg>t</svg>g<math>m</math>h<noscript>n</noscript>i", "abcdefghi\n"},
		{"T-ADJ-61/javascript-link-keeps-text", "<a href=\"javascript:alert(1)\">click</a> <a href=\" JAVASCRIPT:x\">x</a> <a href=\"data:text/html,x\">d</a>", "click x d\n"},
		{"T-ADJ-61/formatting-text-kept", "<p><b>bold</b> <i>it</i> <u>u</u> <s>s</s> <code>c</code></p>", "bold it u s c\n"},
		{"links", "<a href=\"https://example.org/\">https://example.org/</a> <a href=\"mailto:ops@example.org\">ops@example.org</a> <a href=\"https://example.org/x\">x</a>", "https://example.org/ ops@example.org x (https://example.org/x)\n"},
		{"blocks-and-lists", "<h1>Title</h1><p>one\n  two</p><ul><li>a</li><li>b</li></ul>x<br>y<div>z</div>", "Title\n\none two\n\n- a\n- b\nx\ny\nz\n"},
		{"pre-at-most-two-breaks", "0<p><pre>\n0</pre><pre>\n\n\nx\n\n\n\ny</pre>", "0\n\n0\n\nx\n\ny\n"},
		{"pre-keeps-space", "<pre>a   b\n  c</pre>d", "a   b\n  c\nd\n"},
		{"entities", "&lt;tag&gt; &amp; &eacute;", "<tag> & é\n"},
		{"table", "<table><tr><td>a</td><td>b</td></tr><tr><td>c</td></tr></table>", "a b\nc\n"},
		{"unclosed-script", "a<script>b", "a\n"},
		{"T-ADJ-61/self-closing-raw-text-elements", "<p>a</p><script/>alert(1)</script><style/>body{x}</style><TITLE />T</title><iframe/>i</iframe><noscript/>n</noscript><p>b</p>", "a\n\nb\n"},
		{"self-closing-other-elements", "a<svg/>b<math/>c<object/>d<template/>e", "abcde\n"},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := htmlToText(tc.html); got != tc.want {
				t.Errorf("htmlToText() = %q, want %q", got, tc.want)
			}
		})
	}
}

// rawTextTags are forms of the start tag of a raw text element that
// htmlToText drops, each with its end tag.
var rawTextTags = [][2]string{
	{"<script>", "</script>"}, {"<script/>", "</script>"}, {"<SCRIPT type=x >", "</script >"},
	{"<script src=x />", "</SCRIPT>"}, {"<style>", "</style>"}, {"<style/>", "</style>"},
	{"<style media=x/>", "</style>"}, {"<title/>", "</title>"},
}

// startTagAt reports whether the tokenizer, reading doc from the start,
// returns a start or self-closing tag of element at offset.
func startTagAt(doc string, offset int, element atom.Atom) bool {
	z := html.NewTokenizer(strings.NewReader(doc))
	for pos := 0; pos <= offset; {
		tokenType := z.Next()
		if tokenType == html.ErrorToken {
			return false
		}
		if pos == offset {
			name, _ := z.TagName()
			return (tokenType == html.StartTagToken || tokenType == html.SelfClosingTagToken) && atom.Lookup(name) == element
		}
		pos += len(z.Raw())
	}
	return false
}

// FuzzHTMLToText checks that no input panics the converter, that its
// output is valid UTF-8 with at most two line breaks in a row, and that
// the content of a script, style or title element, in any form of its
// start tag, never reaches the output: the document with a marker as that
// content converts exactly as the one with empty content.
func FuzzHTMLToText(f *testing.F) {
	for _, seed := range []string{"<p>a</p>", "<a href='https://x'>y</a>", "<script>x", "<svg><svg></svg>", "<pre>\n x</pre>", "&amp;&#0;"} {
		f.Add(seed, seed, uint8(0))
	}
	f.Add("<p>a</p>", "<p>b</p>", uint8(1))
	f.Fuzz(func(t *testing.T, prefix, suffix string, form uint8) {
		got := htmlToText(prefix + suffix)
		if !utf8.ValidString(got) && utf8.ValidString(prefix+suffix) {
			t.Errorf("output is not valid UTF-8: %q", got)
		}
		if strings.Contains(got, "\n\n\n") {
			t.Errorf("more than two line breaks: %q", got)
		}
		tag := rawTextTags[int(form)%len(rawTextTags)]
		doc := prefix + tag[0] + "MARKER" + tag[1] + suffix
		name, _, _ := strings.Cut(strings.ToLower(strings.Trim(tag[0], "<>/")), " ")
		if !startTagAt(doc, len(prefix), atom.Lookup([]byte(strings.TrimSuffix(name, "/")))) {
			return
		}
		if withMarker, empty := htmlToText(doc), htmlToText(prefix+tag[0]+tag[1]+suffix); withMarker != empty {
			t.Errorf("content of %s reaches the output: %q, without it %q", tag[0], withMarker, empty)
		}
	})
}

type bodyCase struct {
	input string
	want  string
}

func (tc bodyCase) check(t *testing.T) {
	t.Helper()
	msg, warnings := readMessage(t, tc.input)
	if msg.Body != tc.want {
		t.Errorf("Body = %q, want %q", msg.Body, tc.want)
	}
	if !utf8.ValidString(msg.Body) {
		t.Errorf("Body is not valid UTF-8: %q", msg.Body)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %q", warnings)
	}
}

type subjectCase struct {
	input string
	want  string
}

func (tc subjectCase) check(t *testing.T) {
	t.Helper()
	msg, _ := readMessage(t, tc.input)
	if msg.Subject != tc.want {
		t.Errorf("Subject = %q, want %q", msg.Subject, tc.want)
	}
}

// TestReadCronieANSIFixture pins that the cronie fixture declaring
// ANSI_X3.4-1968 for a Cyrillic UTF-8 body loses no character.
func TestReadCronieANSIFixture(t *testing.T) {
	t.Run("T-CALL-12/cronie-ansi-fixture", func(t *testing.T) {
		input, err := os.ReadFile("../../testdata/callers/cronie-ansi.eml")
		if err != nil {
			t.Fatal(err)
		}
		msg, _ := readMessage(t, string(input))
		_, raw, _ := strings.Cut(string(input), "\n\n")
		if msg.Body != raw || !strings.Contains(msg.Body, "Файловая система  Размер  Использовано") {
			t.Errorf("Body = %q, want %q", msg.Body, raw)
		}
	})
}
