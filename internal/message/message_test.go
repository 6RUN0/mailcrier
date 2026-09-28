package message

import (
	"bytes"
	"errors"
	"io"
	"math"
	"reflect"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"testing"
	"testing/iotest"
	"time"
	"unicode/utf8"
)

func TestRead(t *testing.T) {
	t.Run("headers-and-body", readCase{"Subject: disk full\nTo: root\nMessage-ID: <1@db1>\n\n/dev/sda1 99%\n", false, "disk full", "<1@db1>", "/dev/sda1 99%\n"}.check)
	t.Run("no-subject", readCase{"To: root\n\nbody\n", false, "", "", "body\n"}.check)
	t.Run("T-MTA-39/not-a-header-block", readCase{"just text\nmore text\n", false, "", "", "just text\nmore text\n"}.check)
	t.Run("T-ADJ-10/empty-input", readCase{"", false, "", "", ""}.check)
	t.Run("T-MTA-07/empty-input-with-i", readCase{"", true, "", "", ""}.check)
	t.Run("T-ADJ-01/crlf-headers", readCase{"From: bob@example.com\r\nTo: alice@example.com\r\nSubject: Test\r\n\r\nHello, world.\n", false, "Test", "", "Hello, world.\n"}.check)
	t.Run("T-MTA-11/crlf-everywhere", readCase{"Subject: s\r\n\r\nline 1\r\nline 2\r\n", false, "s", "", "line 1\nline 2\n"}.check)
	t.Run("T-MTA-11/quoted-printable-cr-run-without-header-end", func(t *testing.T) {
		msg, _, _, err := Read(strings.NewReader("Content-TrAnsfer-EnCoding:quoted-printABle\n=0D=0D\n"), ReadOptions{IgnoreDots: true})
		if err != nil || msg.Body != "\n" {
			t.Errorf("Body = %q, %v, want %q", msg.Body, err, "\n")
		}
	})
	t.Run("T-MTA-11/quoted-printable-cr-run", readCase{"Content-Transfer-Encoding: quoted-printable\n\na=0D=0D=0Ab\n", false, "", "", "a\nb\n"}.check)
	t.Run("T-MTA-11/base64-cr-run", readCase{"Content-Transfer-Encoding: base64\n\nYQ0NCmI=\n", false, "", "", "a\nb"}.check)
	t.Run("T-MTA-11/utf-16le-crlf", readCase{"Content-Type: text/plain; charset=utf-16le\nContent-Transfer-Encoding: base64\n\nYQANAAoAYgA=\n", false, "", "", "a\nb"}.check)
	t.Run("utf-16be-cr-lf-bytes-inside-a-character", readCase{"Content-Type: text/plain; charset=utf-16be\nContent-Transfer-Encoding: base64\n\nDQoAIA==\n", false, "", "", "\u0d0a "}.check)
	t.Run("lone-cr-kept", readCase{"Content-Transfer-Encoding: quoted-printable\n\na=0Db=0D=0A\n", false, "", "", "a\rb\n\n"}.check)
	t.Run("T-MTA-08/headers-only", readCase{"To: root\nSubject: Output from your job 7\n", false, "Output from your job 7", "", ""}.check)
	t.Run("T-MTA-08/headers-only-no-final-newline", readCase{"Subject: s", false, "s", "", ""}.check)
	t.Run("T-MTA-09/last-line-without-newline", readCase{"Subject: s\n\nline 1\nlast", false, "s", "", "line 1\nlast"}.check)
	t.Run("T-ADJ-55/no-space-after-colon", readCase{"Subject:joined\nAgain:joined\n\nbody\n", false, "joined", "", "body\n"}.check)
	t.Run("T-ADJ-55/no-headers", readCase{"no header here\n\nbody\n", false, "", "", "no header here\n\nbody\n"}.check)
	t.Run("T-MTA-10/mbox-envelope-line", readCase{"From root@example.org Sat Sep 27 10:00:00 2026\nSubject: s\n\nbody\n", false, "s", "", "body\n"}.check)
	t.Run("T-MTA-01/dot-ends-input", readCase{"Subject: s\n\nbefore\n.\nafter\n", false, "s", "", "before\n"}.check)
	t.Run("T-MTA-02/dot-crlf-ends-input", readCase{"Subject: s\r\n\r\nbefore\r\n.\r\nafter\r\n", false, "s", "", "before\n"}.check)
	t.Run("T-MTA-03/dot-with-i", readCase{"Subject: s\n\nbefore\n.\nafter\n", true, "s", "", "before\n.\nafter\n"}.check)
	t.Run("T-MTA-04/leading-dot-removed", readCase{"Subject: s\n\n..foo\n...\n", false, "s", "", ".foo\n..\n"}.check)
	t.Run("T-MTA-04/leading-dot-kept-with-i", readCase{"Subject: s\n\n..foo\n", true, "s", "", "..foo\n"}.check)
	t.Run("T-MTA-05/dot-text-kept", readCase{"Subject: s\n\n.foo\n", false, "s", "", ".foo\n"}.check)
	t.Run("dot-in-header-block-ends-input", readCase{"Subject: s\n.\n\nbody\n", false, "s", "", ""}.check)
	t.Run("folded-subject", readCase{"Subject: first\n  second\n\tthird\n\nb\n", false, "first second third", "", "b\n"}.check)
	t.Run("empty-header-block", readCase{"\nbody\n", false, "", "", "body\n"}.check)
	t.Run("leading-continuation-is-body", readCase{" indented\nSubject: s\n\nb\n", false, "", "", " indented\nSubject: s\n\nb\n"}.check)
}

type readCase struct {
	input       string
	ignoreDots  bool
	wantSubject string
	wantID      string
	wantBody    string
}

func (tc readCase) check(t *testing.T) {
	t.Helper()
	msg, _, warnings, err := Read(strings.NewReader(tc.input), ReadOptions{IgnoreDots: tc.ignoreDots, MaxSize: MaxSize})
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if msg.Subject != tc.wantSubject {
		t.Errorf("Subject = %q, want %q", msg.Subject, tc.wantSubject)
	}
	if msg.MessageID != tc.wantID {
		t.Errorf("MessageID = %q, want %q", msg.MessageID, tc.wantID)
	}
	if msg.Body != tc.wantBody {
		t.Errorf("Body = %q, want %q", msg.Body, tc.wantBody)
	}
	if msg.Size != int64(len(tc.input)) {
		t.Errorf("Size = %d, want %d", msg.Size, len(tc.input))
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %q, want none", warnings)
	}
}

// TestReadMalformedHeaders pins that a broken header line after at least
// one field starts the body: the message is delivered as it came, with a
// warning.
func TestReadMalformedHeaders(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		wantBody string
	}{
		{"space-before-colon", "Subject: s\nfoo : bar\nmore\n", "foo : bar\nmore\n"},
		{"empty-name", "Subject: s\n:foo bar\n\nx\n", ":foo bar\n\nx\n"},
		{"no-colon", "Subject: s\nno colon here\n\nx\n", "no colon here\n\nx\n"},
		{"non-ascii-name", "Subject: s\nThéme: x\n\nx\n", "Théme: x\n\nx\n"},
	}
	for _, tc := range cases {
		t.Run("T-MTA-38/"+tc.name, func(t *testing.T) {
			msg, _, warnings, err := Read(strings.NewReader(tc.input), ReadOptions{MaxSize: MaxSize})
			if err != nil {
				t.Fatal(err)
			}
			if msg.Subject != "s" || msg.Body != tc.wantBody {
				t.Errorf("Subject, Body = %q, %q, want %q, %q", msg.Subject, msg.Body, "s", tc.wantBody)
			}
			if !slices.Equal(warnings, []string{WarningMalformedHeader}) {
				t.Errorf("warnings = %q, want the malformed header warning", warnings)
			}
		})
	}
}

func TestReadAddresses(t *testing.T) {
	input := "From: \"(Cron Daemon)\" <root>\n" +
		"To: root, ops@example.org\n" +
		"Cc: Dev Team <dev@example.org>\n" +
		"Bcc: audit@example.org,\n" +
		" Archive <archive@example.org>\n" +
		"Subject: s\n\nb\n"
	msg, blind, _, err := Read(strings.NewReader(input), ReadOptions{MaxSize: MaxSize})
	if err != nil {
		t.Fatal(err)
	}
	if want := (Address{Name: "(Cron Daemon)", Addr: "root"}); msg.From != want {
		t.Errorf("From = %+v, want %+v", msg.From, want)
	}
	if want := []Address{{Addr: "root"}, {Addr: "ops@example.org"}}; !reflect.DeepEqual(msg.To, want) {
		t.Errorf("To = %+v, want %+v", msg.To, want)
	}
	if want := []Address{{Name: "Dev Team", Addr: "dev@example.org"}}; !reflect.DeepEqual(msg.Cc, want) {
		t.Errorf("Cc = %+v, want %+v", msg.Cc, want)
	}
	t.Run("T-MTA-15/bcc-removed-with-continuation", func(t *testing.T) {
		if want := []Address{{Addr: "audit@example.org"}, {Name: "Archive", Addr: "archive@example.org"}}; !reflect.DeepEqual(blind.Bcc, want) {
			t.Errorf("Bcc = %+v, want %+v", blind.Bcc, want)
		}
		if _, ok := msg.Header["Bcc"]; ok {
			t.Errorf("Header keeps Bcc: %v", msg.Header)
		}
		for key, values := range msg.Header {
			if strings.Contains(strings.Join(values, ","), "archive") {
				t.Errorf("Header %s keeps the Bcc continuation: %q", key, values)
			}
		}
		if msg.Body != "b\n" {
			t.Errorf("Body = %q", msg.Body)
		}
	})
}

func TestParseAddressList(t *testing.T) {
	t.Run("T-CALL-30/quoted-name-without-domain", parseAddressListCase{`"(Cron Daemon)" <root>`, []Address{{Name: "(Cron Daemon)", Addr: "root"}}}.check)
	t.Run("T-CALL-30/comment-name-without-domain", parseAddressListCase{"root (Cron Daemon)", []Address{{Name: "Cron Daemon", Addr: "root"}}}.check)
	t.Run("bare-local-part", parseAddressListCase{"root", []Address{{Addr: "root"}}}.check)
	t.Run("name-and-address", parseAddressListCase{"mdadm monitoring <root>", []Address{{Name: "mdadm monitoring", Addr: "root"}}}.check)
	t.Run("full-address", parseAddressListCase{"Fail2Ban <fail2ban@example.org>", []Address{{Name: "Fail2Ban", Addr: "fail2ban@example.org"}}}.check)
	t.Run("mixed-list", parseAddressListCase{`root, "Doe, Jane" <jane@example.org>, bob (Bob B)`, []Address{{Addr: "root"}, {Name: "Doe, Jane", Addr: "jane@example.org"}, {Name: "Bob B", Addr: "bob"}}}.check)
	t.Run("group", parseAddressListCase{"Team: a@example.org, b@example.org;", []Address{{Addr: "a@example.org"}, {Addr: "b@example.org"}}}.check)
	t.Run("empty-group", parseAddressListCase{"undisclosed-recipients:;", []Address{}}.check)
	t.Run("empty", parseAddressListCase{"  ", nil}.check)
	t.Run("trailing-comma", parseAddressListCase{"a@example.org,", []Address{{Addr: "a@example.org"}}}.check)
	t.Run("unclosed-angle", parseAddressListCase{"Name <root", []Address{{Name: "Name", Addr: "root"}}}.check)
}

type parseAddressListCase struct {
	value string
	want  []Address
}

func (tc parseAddressListCase) check(t *testing.T) {
	t.Helper()
	got := ParseAddressList(tc.value)
	if len(got) == 0 && len(tc.want) == 0 {
		return
	}
	if !reflect.DeepEqual(got, tc.want) {
		t.Errorf("ParseAddressList(%q) = %+v, want %+v", tc.value, got, tc.want)
	}
}

// TestReadSizeLimit pins the input limit: exactly MaxSize bytes pass
// unchanged; beyond it the rest is read and discarded, with a warning.
func TestReadSizeLimit(t *testing.T) {
	const limit = 64
	header := "Subject: s\n\n"
	exact := header + strings.Repeat("x", limit-len(header))
	cases := []struct {
		name         string
		input        string
		wantBody     string
		wantWarnings []string
	}{
		{"T-ADJ-32/exactly-the-limit", exact, exact[len(header):], nil},
		{"T-ADJ-32/over-the-limit", exact + "tail that goes", exact[len(header):], []string{WarningTruncated}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reader := strings.NewReader(tc.input)
			msg, _, warnings, err := Read(reader, ReadOptions{IgnoreDots: true, MaxSize: limit})
			if err != nil {
				t.Fatal(err)
			}
			if msg.Body != tc.wantBody {
				t.Errorf("Body = %q, want %q", msg.Body, tc.wantBody)
			}
			if !slices.Equal(warnings, tc.wantWarnings) {
				t.Errorf("warnings = %q, want %q", warnings, tc.wantWarnings)
			}
			if reader.Len() != 0 {
				t.Errorf("%d bytes left unread", reader.Len())
			}
			if msg.Size != int64(len(tc.input)) {
				t.Errorf("Size = %d, want %d", msg.Size, len(tc.input))
			}
		})
	}
}

func TestReadReportsInputError(t *testing.T) {
	cause := errors.New("broken pipe")
	cases := []struct {
		name   string
		reader io.Reader
	}{
		{"before-the-limit", iotest.ErrReader(cause)},
		{"while-discarding", io.MultiReader(strings.NewReader(strings.Repeat("x", 10)), iotest.ErrReader(cause))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, _, err := Read(tc.reader, ReadOptions{MaxSize: 4}); !errors.Is(err, cause) {
				t.Fatalf("Read() error = %v, want %v", err, cause)
			}
		})
	}
}

// FuzzRead checks that no input panics Read and that the result keeps its
// promises: LF line ends only, no Bcc header, text in valid UTF-8, the
// input size counted.
func FuzzRead(f *testing.F) {
	for _, seed := range []string{
		"Subject: s\n\nb\n", "From x\nTo: a\n.\n", "Bcc: a,\n b\n\n", "resent-bcc: a\n\n", " x\n", "\r\n\r\n", "a:b\n\n..x\n.\r\n",
		"Subject: =?utf-8?q?a=C3?= =?x?B?!!?=\n\n\xff\n",
		"Content-Type: multipart/mixed; boundary=b\n\n--b\nContent-Type: text/html\n\n<a href=x>y</a>\n--b\nContent-Transfer-Encoding: base64\n\nQQ==\n--b--\n",
		"Content-Type: text/plain; charset=koi8-r\nContent-Transfer-Encoding: quoted-printable\n\n=F0=\n=ZZ\n",
	} {
		f.Add(seed, false)
		f.Add(seed, true)
	}
	f.Fuzz(func(t *testing.T, input string, ignoreDots bool) {
		msg, _, _, err := Read(strings.NewReader(input), ReadOptions{IgnoreDots: ignoreDots, MaxSize: 1 << 16})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(msg.Body, "\r\n") {
			t.Errorf("Body keeps CRLF: %q", msg.Body)
		}
		if msg.Header.Has("Bcc") || msg.Header.Has("Resent-Bcc") {
			t.Errorf("Header keeps Bcc or Resent-Bcc")
		}
		if msg.Size != int64(len(input)) {
			t.Errorf("Size = %d, want %d", msg.Size, len(input))
		}
		if !utf8.ValidString(msg.Body) || !utf8.ValidString(msg.BodyHTML) || !utf8.ValidString(msg.Subject) {
			t.Errorf("text is not valid UTF-8: %q, %q, %q", msg.Subject, msg.Body, msg.BodyHTML)
		}
		for name, values := range msg.Header {
			for _, value := range values {
				if !utf8.ValidString(value) {
					t.Errorf("header %s is not valid UTF-8: %q", name, value)
				}
			}
		}
		again, _, _, err := Read(bytes.NewReader(msg.Raw), ReadOptions{IgnoreDots: true, MaxSize: 1 << 16})
		if err != nil {
			t.Fatal(err)
		}
		msg.Size, again.Size = 0, 0
		if !reflect.DeepEqual(again, msg) {
			t.Errorf("Raw read again differs:\n%+v\nwant\n%+v", again, msg)
		}
	})
}

// allocatedBytes returns the bytes Read allocates for input.
func allocatedBytes(t *testing.T, input string) uint64 {
	t.Helper()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	if _, _, _, err := Read(strings.NewReader(input), ReadOptions{MaxSize: MaxSize}); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// readDuration returns the time of one Read over input, averaged over a
// batch of runs that together take at least 20 ms, as testing.Benchmark
// does, so that timer and scheduler noise stay small against the batch
// however short one run is.
func readDuration(t *testing.T, input string) time.Duration {
	t.Helper()
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	runs := 0
	start := time.Now()
	for time.Since(start) < 20*time.Millisecond {
		if _, _, _, err := Read(strings.NewReader(input), ReadOptions{MaxSize: MaxSize}); err != nil {
			t.Fatal(err)
		}
		runs++
	}
	return time.Since(start) / time.Duration(runs)
}

// TestReadIsLinear pins that the allocations and the time of Read grow
// linearly with inputs that stress each part of the reader: a folded
// header, many fields, long address lists, many Bcc lines, many body
// lines, MIME structure and HTML; the header blocks of the larger inputs
// exceed 1 MiB. Four times the input may cost about four times the
// allocations and the time, a quadratic step would cost sixteen, so the
// test allows eight for allocations and ten for time.
//
// A quadratic step that allocates nothing, such as rescanning the text
// of every enclosing link, shows only in the time. The time is measured
// first, from the smallest input, doubling, whose Read takes at least
// 5 ms, so that a quadratic step, which reaches 5 ms early, fails by the
// ratio in seconds and not by the test timeout on the inputs of the
// allocations. The batches of both sizes alternate, so that a change of
// the machine load hits both, and the fastest of three counts. Cache and
// memory bandwidth shared with other processes still put about one ratio
// in a hundred over the limit on a loaded machine, so an exceeded limit
// is measured again, up to three times: a quadratic step exceeds it every
// time.
func TestReadIsLinear(t *testing.T) {
	cases := []struct {
		name  string
		input func(n int) string
	}{
		{"folded-header", func(n int) string { return "Subject: a\n" + strings.Repeat(" bbbbbbbb\n", n) + "\nbody\n" }},
		{"many-fields", func(n int) string { return strings.Repeat("X-A: b\n", n) + "\nbody\n" }},
		{"long-address-list", func(n int) string { return "To: " + strings.Repeat("a@example.org, ", n) + "b\n\nbody\n" }},
		{"loose-address-list", func(n int) string { return "To: " + strings.Repeat("root (x), ", n) + "b\n\nbody\n" }},
		{"many-bcc-lines", func(n int) string {
			return "Bcc: a@example.org,\n" + strings.Repeat(" c@example.org,\n", n) + " d\n\nbody\n"
		}},
		{"body-lines", func(n int) string { return "Subject: a\n\n" + strings.Repeat(".x\n", n) }},
		{"encoded-words", func(n int) string { return "Subject: " + strings.Repeat("=?utf-8?q?a=C3=A9?= ", n) + "\n\nbody\n" }},
		{"malformed-encoded-words", func(n int) string { return "Subject: " + strings.Repeat("=?a?B?!", n) + "?=\n\nbody\n" }},
		{"latin1-header", func(n int) string { return "Subject: " + strings.Repeat("caf\xe9 ", n) + "\n\nbody\n" }},
		{"content-type-parameters", func(n int) string {
			return "Content-Type: text/plain; " + strings.Repeat("a=b; ", n) + "charset=utf-8\n\nbody\n"
		}},
		{"multipart-parts", func(n int) string {
			return "Content-Type: multipart/mixed; boundary=b\n\n" + strings.Repeat("--b\n\nx\n", n) + "--b--\n"
		}},
		{"boundary-prefixes", func(n int) string {
			return "Content-Type: multipart/mixed; boundary=b\n\n--b\n\n" + strings.Repeat("--bx\n", n) + "--b--\n"
		}},
		{"base64-body", func(n int) string {
			return "Content-Transfer-Encoding: base64\n\n" + strings.Repeat("aGVsbG8gd29y\n", n)
		}},
		{"quoted-printable-body", func(n int) string {
			return "Content-Transfer-Encoding: quoted-printable\n\n" + strings.Repeat("=C3=A9t=C3=A9 =\n", n)
		}},
		{"html-nested-links", func(n int) string {
			return "Content-Type: text/html\n\n" + strings.Repeat("<a href=\"https://example.org/\">", n) + "t" + strings.Repeat("</a>", n)
		}},
		{"html-links-around-space", func(n int) string {
			return "Content-Type: text/html\n\n<pre>" + strings.Repeat("<a href=\"https://example.org/\">", n) + strings.Repeat(" ", 16*n) + strings.Repeat("</a>", n)
		}},
		{"html-dropped-elements", func(n int) string {
			return "Content-Type: text/html\n\n" + strings.Repeat("<svg>", n) + strings.Repeat("</svg>", n) + "text"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := 1000
			for readDuration(t, tc.input(n)) < 5*time.Millisecond && n < 1<<20 {
				n *= 2
			}
			smallInput, largeInput := tc.input(n), tc.input(4*n)
			var smallTime, largeTime time.Duration
			for range 3 {
				smallTime, largeTime = time.Duration(math.MaxInt64), time.Duration(math.MaxInt64)
				for range 3 {
					smallTime = min(smallTime, readDuration(t, smallInput))
					largeTime = min(largeTime, readDuration(t, largeInput))
				}
				if largeTime <= 10*smallTime {
					break
				}
			}
			if largeTime > 10*smallTime {
				t.Fatalf("time grows from %v to %v for 4 times the input", smallTime, largeTime)
			}
			small := allocatedBytes(t, tc.input(50000))
			large := allocatedBytes(t, tc.input(200000))
			if large > 8*small {
				t.Errorf("allocations grow from %d to %d bytes for 4 times the input", small, large)
			}
		})
	}
}

// TestReadLargeHeaderKeepsBcc pins that a header block of several MiB is
// read whole: a Bcc field or continuation line far into it is still taken
// out of the message and routes it.
func TestReadLargeHeaderKeepsBcc(t *testing.T) {
	filler := "X-Filler: " + strings.Repeat("a", 1<<20) + "\n"
	cases := []struct {
		name    string
		input   string
		wantBcc []Address
	}{
		{"bcc-field-after-large-field", "Subject: s\n" + filler + "Bcc: secret@example.org\n\nbody\n", []Address{{Addr: "secret@example.org"}}},
		{"bcc-continuation-after-many-lines", "Subject: s\nBcc: first@example.org,\n" + strings.Repeat(" x@example.org,\n", 70000) + " secret@example.org\n\nbody\n", nil},
		{"bcc-continuation-after-large-line", "Subject: s\nBcc: first@example.org\n " + strings.Repeat("x", 1<<20) + "\n secret@example.org\n\nbody\n", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg, blind, warnings, err := Read(strings.NewReader(tc.input), ReadOptions{MaxSize: MaxSize})
			if err != nil {
				t.Fatal(err)
			}
			bcc := blind.Bcc
			if msg.Body != "body\n" || msg.Subject != "s" {
				t.Errorf("Subject %q, body starts %q", msg.Subject, msg.Body[:min(len(msg.Body), 40)])
			}
			if _, ok := msg.Header["Bcc"]; ok {
				t.Error("Header keeps Bcc")
			}
			if len(warnings) != 0 {
				t.Errorf("warnings = %q", warnings)
			}
			if tc.wantBcc != nil && !reflect.DeepEqual(bcc, tc.wantBcc) {
				t.Errorf("bcc = %+v, want %+v", bcc, tc.wantBcc)
			}
			var addresses strings.Builder
			for _, a := range bcc {
				addresses.WriteString(a.Addr + " ")
			}
			if !strings.Contains(addresses.String(), "secret@example.org") {
				t.Errorf("Bcc addresses lack secret@example.org: %d addresses", len(bcc))
			}
		})
	}
}

// TestReadResent pins that the Resent-To, Resent-Cc and Resent-Bcc
// addresses are kept apart from To, Cc and Bcc, that any of these headers,
// even one without addresses, marks the message as resent, that the first
// Resent-From wins, and that Resent-Bcc is removed from the header fields.
func TestReadResent(t *testing.T) {
	t.Run("T-MTA-14/resent-addresses-apart", func(t *testing.T) {
		input := "Resent-From: Fwd <fwd@example.org>\nResent-From: old@example.org\nResent-To: r1@example.org\nResent-Cc: rc@example.org\nResent-To: r2@example.org\n" +
			"Resent-Bcc: Secret <secret@example.org>\nFrom: a@example.org\nTo: t@example.org\nBcc: b@example.org\n\nbody\n"
		msg, blind, _, err := Read(strings.NewReader(input), ReadOptions{MaxSize: MaxSize})
		if err != nil {
			t.Fatal(err)
		}
		if !msg.IsResent {
			t.Error("IsResent = false")
		}
		if want := (Address{Name: "Fwd", Addr: "fwd@example.org"}); msg.ResentFrom != want {
			t.Errorf("ResentFrom = %+v, want %+v", msg.ResentFrom, want)
		}
		if want := []Address{{Addr: "r1@example.org"}, {Addr: "r2@example.org"}}; !reflect.DeepEqual(msg.ResentTo, want) {
			t.Errorf("ResentTo = %+v, want %+v", msg.ResentTo, want)
		}
		if want := []Address{{Addr: "rc@example.org"}}; !reflect.DeepEqual(msg.ResentCc, want) {
			t.Errorf("ResentCc = %+v, want %+v", msg.ResentCc, want)
		}
		want := BlindCopies{Bcc: []Address{{Addr: "b@example.org"}}, ResentBcc: []Address{{Name: "Secret", Addr: "secret@example.org"}}}
		if !reflect.DeepEqual(blind, want) {
			t.Errorf("blind = %+v, want %+v", blind, want)
		}
		if want := []Address{{Addr: "t@example.org"}}; !reflect.DeepEqual(msg.To, want) || msg.From.Addr != "a@example.org" {
			t.Errorf("To = %+v, From = %+v", msg.To, msg.From)
		}
		if got := msg.Header.Names(); slices.Contains(got, "Resent-Bcc") || slices.Contains(got, "Bcc") {
			t.Errorf("Names = %q", got)
		}
	})
	for _, header := range []string{"Resent-To", "Resent-Cc", "Resent-Bcc"} {
		t.Run("T-MTA-14/empty-"+strings.ToLower(header)+"-marks-resent", func(t *testing.T) {
			msg, _, _, err := Read(strings.NewReader("To: a@example.org\n"+header+":\n\nbody\n"), ReadOptions{MaxSize: MaxSize})
			if err != nil {
				t.Fatal(err)
			}
			if !msg.IsResent {
				t.Error("IsResent = false")
			}
		})
	}
	t.Run("resent-from-alone-is-not-resent", func(t *testing.T) {
		msg, _, _, err := Read(strings.NewReader("Resent-From: fwd@example.org\nTo: a@example.org\n\nbody\n"), ReadOptions{MaxSize: MaxSize})
		if err != nil {
			t.Fatal(err)
		}
		if msg.IsResent || msg.ResentFrom.Addr != "fwd@example.org" {
			t.Errorf("IsResent = %v, ResentFrom = %+v", msg.IsResent, msg.ResentFrom)
		}
	})
}

// TestReadDropsControlAddresses pins that an address carrying a control
// character, such as a bare CR that would split a header later, is
// dropped with a warning; the other addresses stay.
func TestReadDropsControlAddresses(t *testing.T) {
	input := "From: <evil\rX-Injected: 1>\nTo: <a\rb@example.org>, ok@example.org\nCc: \"Na\x01me\" <c@example.org>\nBcc: d\x7f@example.org, e@example.org\n\nbody\n"
	msg, blind, warnings, err := Read(strings.NewReader(input), ReadOptions{MaxSize: MaxSize})
	if err != nil {
		t.Fatal(err)
	}
	bcc := blind.Bcc
	if msg.From != (Address{}) || !reflect.DeepEqual(msg.To, []Address{{Addr: "ok@example.org"}}) || len(msg.Cc) != 0 || !reflect.DeepEqual(bcc, []Address{{Addr: "e@example.org"}}) {
		t.Errorf("From %+v, To %+v, Cc %+v, bcc %+v", msg.From, msg.To, msg.Cc, bcc)
	}
	if !slices.Equal(warnings, []string{WarningControlAddress}) {
		t.Errorf("warnings = %q", warnings)
	}
}

// TestParseAddressListLong pins that a list too long for net/mail yields
// every mailbox, names and addresses without domain included.
func TestParseAddressListLong(t *testing.T) {
	const count = 20000
	value := strings.Repeat("Ops Team <ops@example.org>, root (Cron Daemon), ", count)
	if len(value) <= maxStrictListLength {
		t.Fatalf("value of %d bytes does not exceed maxStrictListLength", len(value))
	}
	got := ParseAddressList(value)
	if len(got) != 2*count {
		t.Fatalf("got %d addresses, want %d", len(got), 2*count)
	}
	want := []Address{{Name: "Ops Team", Addr: "ops@example.org"}, {Name: "Cron Daemon", Addr: "root"}}
	if !reflect.DeepEqual(got[len(got)-2:], want) {
		t.Errorf("last addresses = %+v, want %+v", got[len(got)-2:], want)
	}
}
