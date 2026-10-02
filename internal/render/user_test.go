package render

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"text/template"
	"time"

	"github.com/6RUN0/slendmail/internal/message"
	"github.com/6RUN0/slendmail/internal/text"
)

// mustParse parses a template from the configuration for format.
func mustParse(t *testing.T, source string, format text.Format) *Template {
	t.Helper()
	tmpl, err := Parse("custom", source, format)
	if err != nil {
		t.Fatal(err)
	}
	return tmpl
}

// templateCause returns the cause of a *TemplateError, failing the test
// for any other error.
func templateCause(t *testing.T, err error) error {
	t.Helper()
	var templateErr *TemplateError
	if !errors.As(err, &templateErr) {
		t.Fatalf("err = %v (%T), want *TemplateError", err, err)
	}
	return templateErr.Err
}

func TestParseRejectsNesting(t *testing.T) {
	for _, tc := range []struct {
		name, source, want string
	}{
		{"define", "a\n{{ define \"x\" }}b{{ end }}", `template: custom:2:`},
		{"template", "a\n{{ if .Subject }}{{ template \"x\" . }}{{ end }}", `template: custom:2:`},
		{"block", "{{ range .To }}{{ block \"x\" . }}b{{ end }}{{ end }}", `template: custom:1:`},
		{"template-in-else", "{{ with .Subject }}a{{ else }}{{ template \"custom\" }}{{ end }}", `template: custom:1:`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse("custom", tc.source, text.FormatPlain)
			if err == nil || !strings.HasPrefix(err.Error(), tc.want) || !strings.Contains(err.Error(), "not allowed") {
				t.Errorf("err = %v, want %q... not allowed", err, tc.want)
			}
		})
	}
	t.Run("syntax-error-has-position", func(t *testing.T) {
		_, err := Parse("custom", "a\n{{ .Subject ", text.FormatPlain)
		if err == nil || !strings.HasPrefix(err.Error(), "template: custom:2:") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("unknown-function", func(t *testing.T) {
		if _, err := Parse("custom", "{{ now }}", text.FormatPlain); err == nil {
			t.Error("now is accepted")
		}
	})
}

func TestUserTemplateHeaders(t *testing.T) {
	d := readData(t, "Subject: s\nX-Cron-Env: <SHELL=/bin/sh>\nx-cron-env: <HOME=/root>\nX-Mailer-Id: 42\n\nbody\n")
	t.Run("T-TPL-03/dashed-headers-case-insensitive", func(t *testing.T) {
		tmpl := mustParse(t, `{{ .Headers.Get "X-MAILER-ID" }}|{{ .Headers.Values "X-Cron-Env" | join ";" }}`, text.FormatPlain)
		out, err := tmpl.Execute(d)
		if want := "42|<SHELL=/bin/sh>;<HOME=/root>"; err != nil || out != want {
			t.Errorf("got %q, %v, want %q", out, err, want)
		}
	})
}

func TestUserTemplateFailures(t *testing.T) {
	d := Data{Subject: "s", Body: "body", Strings: DefaultStrings()}
	t.Run("T-TPL-05/execution-error", func(t *testing.T) {
		_, err := mustParse(t, `{{ index .To 3 }}`, text.FormatPlain).Execute(d)
		if cause := templateCause(t, err); !strings.Contains(cause.Error(), "index") {
			t.Errorf("cause = %v", cause)
		}
	})
	t.Run("T-ADJ-58/blank-output", func(t *testing.T) {
		_, err := mustParse(t, "{{ .Headers.Get \"X-None\" }} \n\t", text.FormatPlain).Execute(d)
		if cause := templateCause(t, err); !errors.Is(cause, errEmpty) {
			t.Errorf("cause = %v", cause)
		}
	})
	t.Run("panic", func(t *testing.T) {
		tmpl, err := parseUser("custom", "{{ boom }}", text.FormatPlain, template.FuncMap{"boom": func() string { panic("boom") }})
		if err != nil {
			t.Fatal(err)
		}
		_, err = tmpl.Execute(d)
		if cause := templateCause(t, err); !strings.Contains(cause.Error(), "boom") {
			t.Errorf("cause = %v", cause)
		}
	})
	t.Run("bytes", func(t *testing.T) {
		_, err := mustParse(t, " ", text.FormatPlain).ExecuteBytes(d)
		if cause := templateCause(t, err); !errors.Is(cause, errEmpty) {
			t.Errorf("cause = %v", cause)
		}
	})
	t.Run("builtin-not-wrapped", func(t *testing.T) {
		tmpl, err := parse("custom", "{{ index .To 3 }}", text.FormatPlain)
		if err != nil {
			t.Fatal(err)
		}
		var templateErr *TemplateError
		if _, err := tmpl.Execute(d); err == nil || errors.As(err, &templateErr) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("limit-too-small", func(t *testing.T) {
		tmpl := mustParse(t, `{"note": "a fixed text longer than the limit", "body": {{ toJson .Body }}}`, text.FormatGenericJSON)
		_, _, err := Fit(tmpl, d, 10, text.RuneCount)
		if cause := templateCause(t, err); !errors.Is(cause, ErrLimitTooSmall) || !errors.Is(err, ErrLimitTooSmall) {
			t.Errorf("cause = %v", cause)
		}
	})
}

// TestUserTemplateTimeout pins the time limit: the execution that runs
// out of time fails, the template fails at once from then on, and the
// goroutine left behind ends at its next write, without running the rest
// of the template.
func TestUserTemplateTimeout(t *testing.T) {
	release := make(chan struct{})
	isPastWrite := make(chan struct{}, 1)
	tmpl, err := parseUser("custom", "{{ wait }}after{{ mark }}", text.FormatPlain, template.FuncMap{
		"wait": func() string { <-release; return "x" },
		"mark": func() string { isPastWrite <- struct{}{}; return "" },
	})
	if err != nil {
		t.Fatal(err)
	}
	tmpl.user.timeout = time.Millisecond
	d := Data{Body: "body"}
	t.Run("T-TPL-06/timeout", func(t *testing.T) {
		if _, err := tmpl.Execute(d); !errors.Is(templateCause(t, err), errTimeout) {
			t.Errorf("err = %v", err)
		}
		if _, err := tmpl.Execute(d); !errors.Is(templateCause(t, err), errBroken) {
			t.Errorf("second execution: err = %v", err)
		}
		close(release)
		tmpl.user.running.Wait()
		select {
		case <-isPastWrite:
			t.Error("the execution went on past the time limit")
		default:
		}
	})
}

func TestUserTemplateOutputLimit(t *testing.T) {
	big := strings.Repeat("0123456789abcdef\n", (userMaxOutput+1)/17+1)
	d := Data{Subject: "s", Body: big, Strings: DefaultStrings()}
	tmpl := mustParse(t, "{{ .Subject }}\n{{ .Body }}", text.FormatPlain)
	t.Run("T-TPL-06/output-limit", func(t *testing.T) {
		if _, err := tmpl.Execute(d); !errors.Is(templateCause(t, err), errOutputLimit) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("over-limit-cut-by-fit", func(t *testing.T) {
		out, truncated, err := Fit(tmpl, d, 4096, text.RuneCount)
		if err != nil || !truncated || text.RuneCount(out) > 4096 || !strings.HasPrefix(out, "s\n0123") {
			t.Errorf("Fit = %d characters, %v, %v", text.RuneCount(out), truncated, err)
		}
	})
	t.Run("within-limit", func(t *testing.T) {
		small := mustParse(t, "{{ .Body }}", text.FormatPlain)
		small.user.maxOutput = 4
		if out, err := small.Execute(Data{Body: "1234"}); err != nil || out != "1234" {
			t.Errorf("got %q, %v", out, err)
		}
		if _, err := small.Execute(Data{Body: "12345"}); !errors.Is(err, errOutputLimit) {
			t.Errorf("err = %v", err)
		}
	})
}

// functionData is the data of the function cases.
func functionData() Data {
	return Data{
		Subject: "disk failure on sda",
		Body:    "one\ntwo\nthree\n",
		To:      []message.Address{{Name: "Ops", Addr: "ops@example.org"}, {Addr: "dev@example.org"}},
		Date:    time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC),
	}
}

// functionCase is a template from the configuration and its output for
// functionData.
type functionCase struct {
	source, want string
}

func (c functionCase) check(t *testing.T) {
	out, err := mustParse(t, c.source, text.FormatPlain).Execute(functionData())
	if err != nil || out != c.want {
		t.Errorf("got %q, %v, want %q", out, err, c.want)
	}
}

// functionFailure is a template from the configuration whose execution
// fails for functionData, with want as the cause when not nil.
type functionFailure struct {
	source string
	want   error
}

func (c functionFailure) check(t *testing.T) {
	_, err := mustParse(t, c.source, text.FormatPlain).Execute(functionData())
	if cause := templateCause(t, err); c.want != nil && !errors.Is(cause, c.want) {
		t.Errorf("cause = %v, want %v", cause, c.want)
	}
}

func TestUserTemplateFunctions(t *testing.T) {
	t.Run("case", functionCase{`{{ toUpper "aB" }} {{ toLower "aB" }} [{{ trimSpace " a " }}] {{ title "disk failure" }}`, "AB ab [a] Disk Failure"}.check)
	t.Run("title-keeps-acronyms", functionCase{`{{ title "SMART error on /dev/sda, iPhone" }}`, "SMART Error On /Dev/Sda, IPhone"}.check)
	t.Run("T-TPL-09/join-addresses", functionCase{`{{ .To | join ", " }}`, "Ops <ops@example.org>, dev@example.org"}.check)
	t.Run("T-TPL-09/join-strings", functionCase{`{{ lines .Body | join "+" }}`, "one+two+three"}.check)
	t.Run("join-empty", functionCase{`[{{ .Cc | join ", " }}]`, "[]"}.check)
	t.Run("join-not-a-list", functionFailure{`{{ join ", " 5 }}`, nil}.check)
	t.Run("match", functionCase{`{{ match "fail(ure)?" .Subject }} {{ match "^ok" .Subject }}`, "true false"}.check)
	t.Run("reReplaceAll", functionCase{`{{ reReplaceAll "s(d[a-z])" "/dev/s$1" .Subject }}`, "disk failure on /dev/sda"}.check)
	t.Run("bad-pattern", functionFailure{`{{ match "(" .Subject }}`, nil}.check)
	t.Run("T-TPL-10/date-in-zone", functionCase{`{{ .Date | tz "Europe/Berlin" | date "2006-01-02 15:04 MST" }}`, "2026-09-27 12:00 CEST"}.check)
	t.Run("T-TPL-10/unknown-zone", functionFailure{`{{ .Date | tz "Mars/Olympus" | date "15:04" }}`, errUnknownZone}.check)
	t.Run("lines", functionCase{`{{ range lines .Body }}[{{ . }}]{{ end }}`, "[one][two][three]"}.check)
	t.Run("head", functionCase{`{{ head 2 .Body }}|{{ head 9 .Body }}|{{ head 0 .Body }}`, "one\ntwo|one\ntwo\nthree|"}.check)
	t.Run("tail", functionCase{`{{ tail 2 .Body }}|{{ tail -1 .Body }}`, "two\nthree|"}.check)
	t.Run("T-TPL-08/truncate", functionCase{`{{ truncate 10 .Subject }}|{{ truncate 4 "ab" }}|{{ truncate 5 "абвгдеж" }}`, "disk fa...|ab|аб..."}.check)
	t.Run("T-TPL-08/truncate-short", functionCase{`{{ truncate 3 .Subject }}|{{ truncate 1 "абв!" }}|[{{ truncate 0 "x" }}]`, "dis|а|[]"}.check)
	t.Run("indent", functionCase{`{{ indent 2 (head 2 .Body) }}`, "  one\n  two"}.check)
}

// TestUserTemplateBudget pins the budget of WithBudget: it ends an
// execution before the time limit of one execution would, it is shared
// by the copy and the template, and a built-in template has none.
func TestUserTemplateBudget(t *testing.T) {
	t.Run("T-TPL-06/budget-ends-execution", func(t *testing.T) {
		release := make(chan struct{})
		tmpl, err := parseUser("custom", "{{ wait }}", text.FormatPlain, template.FuncMap{"wait": func() string { <-release; return "x" }})
		if err != nil {
			t.Fatal(err)
		}
		tmpl.user.timeout, tmpl.user.budget = time.Hour, time.Millisecond
		if _, err := tmpl.WithBudget(time.Time{}).Execute(Data{}); !errors.Is(templateCause(t, err), errTimeout) {
			t.Errorf("err = %v", err)
		}
		if _, err := tmpl.Execute(Data{}); !errors.Is(templateCause(t, err), errBroken) {
			t.Errorf("template after the budget: err = %v", err)
		}
		close(release)
		tmpl.user.running.Wait()
	})
	t.Run("T-TPL-06/deadline-passed", func(t *testing.T) {
		var calls atomic.Int32
		tmpl, err := parseUser("custom", "{{ count }}", text.FormatPlain, template.FuncMap{"count": func() string { calls.Add(1); return "x" }})
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = Fit(tmpl.WithBudget(time.Now().Add(-time.Second)), Data{}, 10, text.RuneCount)
		if !errors.Is(templateCause(t, err), errBudgetSpent) || calls.Load() != 0 {
			t.Errorf("err = %v, %d executions", err, calls.Load())
		}
		if out, err := tmpl.Execute(Data{}); err != nil || out != "x" || tmpl.user.isBroken.Load() {
			t.Errorf("template after a spent budget: %q, %v, broken %v", out, err, tmpl.user.isBroken.Load())
		}
	})
	t.Run("budget-left", func(t *testing.T) {
		tmpl := mustParse(t, "{{ .Subject }}", text.FormatPlain)
		if out, err := tmpl.WithBudget(time.Now().Add(time.Hour)).Execute(Data{Subject: "s"}); err != nil || out != "s" {
			t.Errorf("got %q, %v", out, err)
		}
	})
	t.Run("builtin-unbounded", func(t *testing.T) {
		builtin, err := Builtin(text.FormatPlain)
		if err != nil {
			t.Fatal(err)
		}
		if builtin.WithBudget(time.Now().Add(-time.Second)) != builtin {
			t.Error("WithBudget copies a built-in template")
		}
	})
}
