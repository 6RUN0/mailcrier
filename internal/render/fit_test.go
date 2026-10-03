package render

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/bits"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/6RUN0/mailcrier/internal/text"
)

// measureFunc is a length function of a target.
type measureFunc struct {
	name    string
	measure func(string) int
}

// measures are the length functions of the targets.
var measures = []measureFunc{
	{"telegram-html", text.MeasureTelegramHTML},
	{"utf16", text.UTF16Len},
	{"runes", text.RuneCount},
	{"bytes", text.ByteLen},
}

// fitSubject is the subject of fitData.
const fitSubject = "disk & <raid> . _ *"

// fitData returns data with a subject and body for Fit.
func fitData(body string) Data {
	return Data{Subject: fitSubject, Hostname: "host1.example.org", Body: body, Strings: DefaultStrings()}
}

// checkFitOutput checks what Fit returned for d: a result that measures
// at most the limit, valid JSON for a JSON format, balanced tags and whole
// entities for Telegram HTML, the full rendering when nothing was cut, and
// ErrLimitTooSmall only for a strict format whose shortest rendering is
// too long.
func checkFitOutput(t *testing.T, tmpl *Template, d Data, limit int, m measureFunc, out string, truncated bool, err error) {
	t.Helper()
	if errors.Is(err, ErrLimitTooSmall) {
		short := d
		short.Body = withNotice("", d.Strings.Truncated)
		short.Attachments, short.MoreAttachments = nil, len(d.Attachments)
		for _, kept := range []int{0, 1} {
			short.Subject = ""
			if kept > 0 {
				short.Subject = cutSubject(d.Subject, kept)
			}
			shortest, execErr := tmpl.Execute(short)
			if !strictFormats[tmpl.format] || execErr != nil || m.measure(shortest) <= limit {
				t.Errorf("%s/%s/%d: ErrLimitTooSmall, but the rendering with %d subject characters measures %d: %q", tmpl.format, m.name, limit, kept, m.measure(shortest), shortest)
			}
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if limit > 0 && m.measure(out) > limit {
		t.Errorf("%s/%s/%d: result measures %d: %q", tmpl.format, m.name, limit, m.measure(out), out)
	}
	if !utf8.ValidString(out) {
		t.Errorf("%s/%s/%d: invalid UTF-8: %q", tmpl.format, m.name, limit, out)
	}
	if jsonFormats[tmpl.format] && !json.Valid([]byte(out)) {
		t.Errorf("%s/%s/%d: invalid JSON: %q", tmpl.format, m.name, limit, out)
	}
	if tmpl.format == text.FormatTelegramHTML {
		checkTelegramHTML(t, out)
	}
	if !truncated {
		full, err := tmpl.Execute(d)
		if err != nil || out != full {
			t.Errorf("%s/%s/%d: untruncated result differs from the full rendering", tmpl.format, m.name, limit)
		}
	}
}

// telegramMarkup matches a tag or an entity of the built-in Telegram HTML
// template.
var telegramMarkup = regexp.MustCompile(`</?(?:b|i|pre|blockquote)>|<blockquote expandable>|&(?:amp|lt|gt);`)

// checkTelegramHTML checks that every tag of out is closed in order and
// that no <, > or & stands outside a tag or an entity.
func checkTelegramHTML(t *testing.T, out string) {
	t.Helper()
	var open []string
	for _, markup := range telegramMarkup.FindAllString(out, -1) {
		switch {
		case strings.HasPrefix(markup, "&"):
		case strings.HasPrefix(markup, "</"):
			if len(open) == 0 || open[len(open)-1] != markup[2:] {
				t.Errorf("unbalanced %s in %q", markup, out)
				return
			}
			open = open[:len(open)-1]
		default:
			name, _, _ := strings.Cut(markup[1:len(markup)-1], " ")
			open = append(open, name+">")
		}
	}
	if len(open) > 0 {
		t.Errorf("unclosed %q in %q", open, out)
	}
	if strings.ContainsAny(telegramMarkup.ReplaceAllString(out, ""), "<>&") {
		t.Errorf("bare markup character in %q", out)
	}
}

func TestFit(t *testing.T) {
	body := strings.Repeat("line with & < > . _ * and ёжик 😀\n", 200)
	t.Run("T-LIM-17/every-template-and-unit", func(t *testing.T) {
		checkFitMatrix(t, body)
	})
	t.Run("T-LIM-17/body-cut-with-notice", func(t *testing.T) {
		tmpl, err := Builtin(text.FormatTelegramHTML)
		if err != nil {
			t.Fatal(err)
		}
		out, truncated, err := Fit(tmpl, fitData(strings.Repeat("word & more <x> ", 1000)), 4096, text.MeasureTelegramHTML)
		if err != nil || !truncated {
			t.Fatalf("truncated = %v, err = %v", truncated, err)
		}
		if n := text.MeasureTelegramHTML(out); n > 4096 || n < 4000 {
			t.Errorf("result measures %d, want close to 4096", n)
		}
		if !strings.HasSuffix(out, "\n[truncated]</pre>") || strings.Contains(out, "&am\n") {
			t.Errorf("result ends %q", out[len(out)-60:])
		}
	})
	t.Run("fits-unchanged", func(t *testing.T) {
		tmpl, err := Builtin(text.FormatPlain)
		if err != nil {
			t.Fatal(err)
		}
		full, err := tmpl.Execute(fitData("short\n"))
		if err != nil {
			t.Fatal(err)
		}
		for _, limit := range []int{0, -1, len(full)} {
			out, truncated, err := Fit(tmpl, fitData("short\n"), limit, text.ByteLen)
			if err != nil || truncated || out != full {
				t.Errorf("limit %d: out = %q, truncated = %v, err = %v", limit, out, truncated, err)
			}
		}
	})
	t.Run("long-subject-cut-before-short-body", func(t *testing.T) {
		for _, format := range builtinFormats {
			if format == text.FormatNtfy {
				continue
			}
			tmpl, err := Builtin(format)
			if err != nil {
				t.Fatal(err)
			}
			d := fitData("backup FAILED: disk full\n")
			d.Subject = strings.Repeat("word ", 1000)
			out, truncated, err := Fit(tmpl, d, 4096, text.RuneCount)
			checkFitOutput(t, tmpl, d, 4096, measures[2], out, truncated, err)
			if err != nil || !truncated || !strings.Contains(out, "backup FAILED: disk full") || strings.Contains(out, "[truncated]") {
				t.Errorf("%s: truncated = %v, err = %v, body kept whole: %v", format, truncated, err, strings.Contains(out, "backup FAILED: disk full"))
			}
			if n := strings.Count(out, "word"); n < 150 || n > 4096/4/len("word ")+1 {
				t.Errorf("%s: subject keeps %d words, want at most a quarter of the limit", format, n)
			}
		}
	})
	t.Run("long-subject-cut-with-long-body", func(t *testing.T) {
		tmpl, err := Builtin(text.FormatTelegramHTML)
		if err != nil {
			t.Fatal(err)
		}
		d := fitData(strings.Repeat("body line\n", 1000))
		d.Subject = strings.Repeat("word ", 1000)
		out, truncated, err := Fit(tmpl, d, 4096, text.MeasureTelegramHTML)
		checkFitOutput(t, tmpl, d, 4096, measures[0], out, truncated, err)
		if n := strings.Count(out, "word"); err != nil || !truncated || n > 4096/4/len("word ")+1 || strings.Count(out, "body line") < 200 {
			t.Errorf("truncated = %v, err = %v, %d subject words, %d body lines", truncated, err, n, strings.Count(out, "body line"))
		}
	})
	t.Run("subject-of-one-character-before-empty", func(t *testing.T) {
		tmpl, err := Builtin(text.FormatTelegramHTML)
		if err != nil {
			t.Fatal(err)
		}
		d := fitData(strings.Repeat("b", 100))
		d.Subject = "xyz12345"
		d.Hostname = "h"
		shortest := d
		shortest.Subject, shortest.Body = "x...", withNotice("", d.Strings.Truncated)
		want, err := tmpl.Execute(shortest)
		if err != nil {
			t.Fatal(err)
		}
		limit := text.MeasureTelegramHTML(want)
		out, truncated, err := Fit(tmpl, d, limit, text.MeasureTelegramHTML)
		checkFitOutput(t, tmpl, d, limit, measures[0], out, truncated, err)
		if err != nil || out != want {
			t.Errorf("limit %d: out = %q, err = %v, want %q", limit, out, err, want)
		}
	})
	t.Run("subject-share-at-least-64", func(t *testing.T) {
		tmpl, err := Builtin(text.FormatPlain)
		if err != nil {
			t.Fatal(err)
		}
		d := fitData("b")
		d.Subject = strings.Repeat("word ", 100)
		out, truncated, err := Fit(tmpl, d, 100, text.RuneCount)
		checkFitOutput(t, tmpl, d, 100, measures[2], out, truncated, err)
		subject, _, _ := strings.Cut(out, "\n")
		if n := text.RuneCount(subject); err != nil || n <= 100/4 || n > minSubjectShare || !strings.HasSuffix(subject, subjectCutMark) {
			t.Errorf("subject %q of %d characters, want more than %d and at most %d, marked", subject, n, 100/4, minSubjectShare)
		}
	})
	t.Run("subject-share-in-target-unit", func(t *testing.T) {
		tmpl, err := Builtin(text.FormatPlain)
		if err != nil {
			t.Fatal(err)
		}
		d := fitData(strings.Repeat("body line\n", 100))
		d.Subject = strings.Repeat("ёжик ", 40)
		out, truncated, err := Fit(tmpl, d, 400, text.ByteLen)
		checkFitOutput(t, tmpl, d, 400, measures[3], out, truncated, err)
		subject, _, _ := strings.Cut(out, "\n")
		if n := len(subject); err != nil || n > 400/4 || n < 400/4-len("ёжик ") {
			t.Errorf("subject %q of %d bytes, want close to %d", subject, n, 400/4)
		}
	})
	t.Run("subject-share-after-escaping", func(t *testing.T) {
		tmpl, err := Builtin(text.FormatTelegramHTML)
		if err != nil {
			t.Fatal(err)
		}
		d := fitData(strings.Repeat("word ", 600))
		d.Subject = strings.Repeat("<x> ", 300)
		out, truncated, err := Fit(tmpl, d, 4096, text.MeasureTelegramHTML)
		checkFitOutput(t, tmpl, d, 4096, measures[0], out, truncated, err)
		subject, _, _ := strings.Cut(out, "\n")
		if n := text.MeasureTelegramHTML(subject); err != nil || n > 4096/4 || strings.Count(out, "word") != 600 {
			t.Errorf("subject of %d units, %d body words; want at most %d and all 600", n, strings.Count(out, "word"), 4096/4)
		}
	})
	t.Run("cut-subject-marked", func(t *testing.T) {
		tmpl, err := Builtin(text.FormatTelegramHTML)
		if err != nil {
			t.Fatal(err)
		}
		d := fitData("short body\n")
		d.Subject = strings.Repeat("word ", 1000)
		out, _, err := Fit(tmpl, d, 4096, text.MeasureTelegramHTML)
		if err != nil || !strings.HasPrefix(out, "<b>word") || !strings.Contains(out, "word...</b>") {
			t.Errorf("err = %v, subject line %q", err, out[:min(len(out), 60)])
		}
	})
	t.Run("empty-subject-last", func(t *testing.T) {
		tmpl, err := Builtin(text.FormatGenericJSON)
		if err != nil {
			t.Fatal(err)
		}
		d := fitData(strings.Repeat("b", 100))
		d.Subject = "xyz"
		shortest := d
		shortest.Subject, shortest.Body = "", withNotice("", d.Strings.Truncated)
		want, err := tmpl.Execute(shortest)
		if err != nil {
			t.Fatal(err)
		}
		limit := text.RuneCount(want)
		out, truncated, err := Fit(tmpl, d, limit, text.RuneCount)
		checkFitOutput(t, tmpl, d, limit, measures[2], out, truncated, err)
		if err != nil || out != want {
			t.Errorf("limit %d: out = %q, err = %v, want %q", limit, out, err, want)
		}
	})
	t.Run("strict-by-target-format", func(t *testing.T) {
		source := `{"subject": {{ toJson .Subject }}, "pad": "` + strings.Repeat("x", 200) + `"}`
		strict, err := parse("custom", source, text.FormatGenericJSON)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := Fit(strict, fitData("b"), 50, text.RuneCount); !errors.Is(err, ErrLimitTooSmall) {
			t.Errorf("template for generic-json: err = %v, want ErrLimitTooSmall", err)
		}
		lenient, err := parse("custom", source, text.FormatPlain)
		if err != nil {
			t.Fatal(err)
		}
		if out, _, err := Fit(lenient, fitData("b"), 50, text.RuneCount); err != nil || text.RuneCount(out) != 50 {
			t.Errorf("template for plain: out = %q, err = %v, want a prefix of 50 characters", out, err)
		}
	})
	t.Run("attachment-list-cut", func(t *testing.T) {
		for _, format := range []text.Format{text.FormatTelegramHTML, text.FormatTelegramMarkdownV2} {
			tmpl, err := Builtin(format)
			if err != nil {
				t.Fatal(err)
			}
			d := fitData("short body\n")
			for i := range 150 {
				d.Attachments = append(d.Attachments, Attachment{Name: fmt.Sprintf("report-%03d.log", i), ContentType: "text/plain", Size: 1234})
			}
			m := measures[0]
			if format == text.FormatTelegramMarkdownV2 {
				m = measures[1]
			}
			out, truncated, err := Fit(tmpl, d, 4096, m.measure)
			checkFitOutput(t, tmpl, d, 4096, m, out, truncated, err)
			listed := strings.Count(out, "report")
			more := fmt.Sprintf(d.Strings.MoreAttachments, 150-listed)
			if format == text.FormatTelegramMarkdownV2 {
				more = text.EscapeTelegramMarkdownV2(more)
			}
			if err != nil || !truncated || !strings.Contains(out, "short body") || listed < 50 || listed >= 150 || !strings.HasSuffix(out, "\n"+more+"\n") && !strings.HasSuffix(out, "\n"+more) {
				t.Errorf("%s: truncated = %v, err = %v, %d listed, ends %q", format, truncated, err, listed, out[max(0, len(out)-80):])
			}
		}
	})
	t.Run("attachment-list-and-body-cut", func(t *testing.T) {
		tmpl, err := Builtin(text.FormatTelegramHTML)
		if err != nil {
			t.Fatal(err)
		}
		d := fitData(strings.Repeat("body line\n", 1000))
		for i := range 150 {
			d.Attachments = append(d.Attachments, Attachment{Name: fmt.Sprintf("report-%03d.log", i), ContentType: "text/plain", Size: 1234})
		}
		out, truncated, err := Fit(tmpl, d, 4096, text.MeasureTelegramHTML)
		checkFitOutput(t, tmpl, d, 4096, measures[0], out, truncated, err)
		if err != nil || !truncated || strings.Contains(out, "report-") || !strings.Contains(out, "[truncated]") || !strings.Contains(out, "... and 150 more") {
			t.Errorf("truncated = %v, err = %v, ends %q", truncated, err, out[max(0, len(out)-80):])
		}
	})
	t.Run("strict-format-too-long-without-subject", func(t *testing.T) {
		for _, format := range []text.Format{text.FormatTelegramMarkdownV2, text.FormatGenericJSON} {
			tmpl, err := Builtin(format)
			if err != nil {
				t.Fatal(err)
			}
			d := fitData("body\n")
			d.Hostname = strings.Repeat("h", 5000)
			if _, _, err := Fit(tmpl, d, 4096, text.RuneCount); !errors.Is(err, ErrLimitTooSmall) {
				t.Errorf("%s: err = %v, want ErrLimitTooSmall", format, err)
			}
		}
	})
	t.Run("T-LIM-16/telegram-wrapper-longer-than-limit-cut-hard", func(t *testing.T) {
		tmpl, err := Builtin(text.FormatTelegramHTML)
		if err != nil {
			t.Fatal(err)
		}
		d := fitData("body\n")
		d.Hostname = strings.Repeat("h&", 2500)
		out, truncated, err := Fit(tmpl, d, 4096, text.MeasureTelegramHTML)
		checkFitOutput(t, tmpl, d, 4096, measures[0], out, truncated, err)
		if err != nil || !truncated || text.MeasureTelegramHTML(out) < 4000 || !strings.HasSuffix(out, "h&amp;</i>") {
			t.Errorf("truncated = %v, err = %v, %d characters, ends %q", truncated, err, text.MeasureTelegramHTML(out), out[max(0, len(out)-40):])
		}
	})
	t.Run("wrapper-longer-than-limit", func(t *testing.T) {
		tmpl, err := parse("wrapper", strings.Repeat("header ", 100)+"{{ .Body }}", text.FormatPlain)
		if err != nil {
			t.Fatal(err)
		}
		out, truncated, err := Fit(tmpl, fitData("body"), 20, text.RuneCount)
		if err != nil || !truncated || out != "header header header" {
			t.Errorf("out = %q, truncated = %v, err = %v", out, truncated, err)
		}
	})
}

// TestFitAtTheLimit pins the boundary of the Telegram limit with the
// built-in template: a text that measures exactly the limit, wrapper
// included, goes as it is; one character more cuts the body at the last
// white space within 100 characters, or hard without one, and ends it with
// the notice, within the limit.
func TestFitAtTheLimit(t *testing.T) {
	tmpl, err := Builtin(text.FormatTelegramHTML)
	if err != nil {
		t.Fatal(err)
	}
	const limit = 4096
	wrapper, err := tmpl.Execute(fitData(""))
	if err != nil {
		t.Fatal(err)
	}
	room := limit - text.MeasureTelegramHTML(wrapper) + text.RuneCount(DefaultStrings().EmptyBody)
	check := func(word string) func(*testing.T) {
		return func(t *testing.T) {
			body := text.TruncateRunes(strings.Repeat(word, room), room)
			out, truncated, err := Fit(tmpl, fitData(body), limit, text.MeasureTelegramHTML)
			if err != nil || truncated || text.MeasureTelegramHTML(out) != limit {
				t.Fatalf("body at the limit: truncated = %v, err = %v, %d characters", truncated, err, text.MeasureTelegramHTML(out))
			}
			out, truncated, err = Fit(tmpl, fitData(body+"y"), limit, text.MeasureTelegramHTML)
			checkFitOutput(t, tmpl, fitData(body+"y"), limit, measures[0], out, truncated, err)
			kept, isCut := strings.CutSuffix(out, "\n[truncated]</pre>")
			if !truncated || !isCut {
				t.Fatalf("one more character: truncated = %v, ends %q", truncated, out[max(0, len(out)-60):])
			}
			_, keptBody, _ := strings.Cut(kept, "<pre>")
			raw := strings.NewReplacer("&lt;", "<", "&gt;", ">", "&amp;", "&").Replace(keptBody)
			lost := text.RuneCount(body) - text.RuneCount(raw)
			if !strings.HasPrefix(body, raw) || lost > 100+len("[truncated]")+1 {
				t.Errorf("kept %d of %d characters, %d lost", text.RuneCount(raw), text.RuneCount(body), lost)
			}
			if strings.Contains(word, " ") && (strings.HasSuffix(raw, " ") || !strings.HasPrefix(body[len(raw):], " ")) {
				t.Errorf("cut not at a word: ends %q", raw[max(0, len(raw)-20):])
			}
		}
	}
	t.Run("T-ADJ-42/cut-at-word", check("word & <x> "))
	t.Run("T-ADJ-42/cut-hard-without-space", check("x"))
}

// TestFitLines pins max_lines: a body of more lines is cut after the last
// line allowed and marked, even when the rest would fit the length limit.
func TestFitLines(t *testing.T) {
	tmpl, err := Builtin(text.FormatPlain)
	if err != nil {
		t.Fatal(err)
	}
	check := func(body string, maxLines int, wantBody string, wantTruncated bool) func(*testing.T) {
		return func(t *testing.T) {
			out, truncated, err := FitLines(tmpl, fitData(body), 4096, maxLines, text.RuneCount)
			_, got, _ := strings.Cut(out, "\n\n")
			if err != nil || truncated != wantTruncated || got != wantBody {
				t.Errorf("body %q, truncated = %v, err = %v; want %q, %v", got, truncated, err, wantBody, wantTruncated)
			}
		}
	}
	t.Run("T-LIM-19/first-lines-kept", check("one\ntwo\nthree\n", 2, "one\ntwo\n[truncated]", true))
	t.Run("lines-at-the-limit", check("one\ntwo\n", 2, "one\ntwo", false))
	t.Run("trailing-blank-lines-ignored", check("one\ntwo\n\n \n", 2, "one\ntwo", false))
	t.Run("no-limit", check("one\ntwo\nthree\n", 0, "one\ntwo\nthree", false))
	t.Run("then-cut-to-the-length-limit", func(t *testing.T) {
		d := fitData(strings.Repeat("a long line of the body\n", 50))
		out, truncated, err := FitLines(tmpl, d, 100, 40, text.RuneCount)
		checkFitOutput(t, tmpl, d, 100, measures[2], out, truncated, err)
		if !truncated || strings.Count(out, "[truncated]") != 1 {
			t.Errorf("truncated = %v, out %q", truncated, out)
		}
	})
}

// TestFitMeasuresLogarithmically pins the number of renderings: one for
// the full text and at most one per bit of the body length.
func TestFitMeasuresLogarithmically(t *testing.T) {
	tmpl, err := Builtin(text.FormatTelegramHTML)
	if err != nil {
		t.Fatal(err)
	}
	for _, size := range []int{1 << 10, 1 << 16, 1 << 20} {
		body := strings.Repeat("a&<", size/3)
		calls := 0
		measure := func(s string) int {
			calls++
			return text.MeasureTelegramHTML(s)
		}
		if _, _, err := Fit(tmpl, fitData(body), 4096, measure); err != nil {
			t.Fatal(err)
		}
		if limit := 2 + bits.Len(uint(utf8.RuneCountInString(body))); calls > limit {
			t.Errorf("body of %d characters: %d measurements, want at most %d", len(body), calls, limit)
		}
	}
}

// FuzzFit checks the invariants of Fit for any subject, body and limit,
// in every unit and with every built-in template: see checkFitOutput.
// Subject and body are valid UTF-8, as message.Read returns them.
func FuzzFit(f *testing.F) {
	f.Add(fitSubject, "a & b < c > d . e _ f * g", 40, uint8(0), uint8(0))
	f.Add("", strings.Repeat("😀ж", 50), 7, uint8(1), uint8(1))
	f.Add("@here", "```\n@channel\n", 3, uint8(6), uint8(2))
	f.Add(strings.Repeat("&<> ", 300), "b", 300, uint8(1), uint8(0))
	f.Add(strings.Repeat("\"x\\ ", 300), "b", 200, uint8(8), uint8(3))
	f.Fuzz(func(t *testing.T, subject, body string, limit int, format, unit uint8) {
		if !utf8.ValidString(subject) || !utf8.ValidString(body) {
			return
		}
		limit %= 5000
		tmpl, err := Builtin(builtinFormats[int(format)%len(builtinFormats)])
		if err != nil {
			t.Fatal(err)
		}
		m := measures[int(unit)%len(measures)]
		d := fitData(body)
		d.Subject = subject
		out, truncated, err := Fit(tmpl, d, limit, m.measure)
		checkFitOutput(t, tmpl, d, limit, m, out, truncated, err)
	})
}

// checkFitMatrix fits body with every built-in template, unit and a range
// of limits, and checks that each result measures at most the limit.
func checkFitMatrix(t *testing.T, body string) {
	t.Helper()
	for _, format := range builtinFormats {
		tmpl, err := Builtin(format)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range measures {
			for _, limit := range []int{1, 50, 300, 1000, 4096} {
				out, truncated, err := Fit(tmpl, fitData(body), limit, m.measure)
				checkFitOutput(t, tmpl, fitData(body), limit, m, out, truncated, err)
				if err == nil && !truncated {
					t.Errorf("%s/%s/%d: not truncated", format, m.name, limit)
				}
			}
		}
	}
}
