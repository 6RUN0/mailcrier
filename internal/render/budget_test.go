package render_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"text/template"
	"time"

	"github.com/6RUN0/slendmail/internal/backend"
	"github.com/6RUN0/slendmail/internal/delivery"
	"github.com/6RUN0/slendmail/internal/render"
	"github.com/6RUN0/slendmail/internal/text"
)

// recordingSender takes every payload and records the texts.
type recordingSender struct {
	caps  backend.Caps
	texts []string
}

func (s *recordingSender) Caps() backend.Caps { return s.caps }

func (s *recordingSender) Send(_ context.Context, p backend.Payload) error {
	s.texts = append(s.texts, p.Text)
	return nil
}

// TestBudgetSharedByBothFits pins that the two renderings of a cut text in
// delivery, the second of which names the size of the full text, draw on
// one budget. Each rendering advances a hand-driven clock by 60% of the
// budget once: the budget is spent only when both share it.
func TestBudgetSharedByBothFits(t *testing.T) {
	const budget = time.Hour
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	var isFirstAdvanced, isSecondAdvanced atomic.Bool
	advance := func(notice string) string {
		isSecond := strings.Contains(notice, "in full")
		flag := &isFirstAdvanced
		if isSecond {
			flag = &isSecondAdvanced
		}
		if flag.CompareAndSwap(false, true) {
			clock.Add(int64(budget * 6 / 10))
		}
		return ""
	}
	now := func() time.Time { return time.Unix(0, clock.Load()) }
	tmpl, err := render.ParseWithClock("{{ advance .Strings.Truncated }}{{ .Subject }}\n{{ .Body }}", text.FormatPlain,
		template.FuncMap{"advance": advance}, budget, now)
	if err != nil {
		t.Fatal(err)
	}
	builtin, err := render.Builtin(text.FormatPlain)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("T-TPL-06/budget-spent-in-second-fit", func(t *testing.T) {
		sender := &recordingSender{caps: backend.Caps{MaxText: 100}}
		target := delivery.Target{ID: "t", Sender: sender, Template: tmpl, Fallback: builtin, OnLong: delivery.OnLongTruncate}
		d := render.Data{Subject: "s", Body: strings.Repeat("word ", 100), Strings: render.DefaultStrings()}
		result := delivery.Deliver(context.Background(), []delivery.Target{target}, d, nil)[0]
		var templateErr *render.TemplateError
		if !isFirstAdvanced.Load() || !isSecondAdvanced.Load() || !errors.As(result.TemplateErr, &templateErr) ||
			!strings.Contains(templateErr.Error(), "budget of the text spent") {
			t.Fatalf("result = %+v, first %v, second %v", result, isFirstAdvanced.Load(), isSecondAdvanced.Load())
		}
		if len(sender.texts) != 1 || strings.HasPrefix(sender.texts[0], "s\nword") {
			t.Errorf("texts = %q, want the built-in text", sender.texts)
		}
	})
}

// TestBudgetSharedWithRequestParts pins that the parts of a request and
// the text draw on one budget: a header that spends 60% of it and a text
// that spends 60% more leave the text to the built-in template.
func TestBudgetSharedWithRequestParts(t *testing.T) {
	const budget = time.Hour
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()) }
	var isPartAdvanced, isTextAdvanced atomic.Bool
	advanceOnce := func(flag *atomic.Bool) func() string {
		return func() string {
			if flag.CompareAndSwap(false, true) {
				clock.Add(int64(budget * 6 / 10))
			}
			return ""
		}
	}
	header, err := render.ParsePartWithClock("{{ advance }}x", template.FuncMap{"advance": advanceOnce(&isPartAdvanced)}, budget, now)
	if err != nil {
		t.Fatal(err)
	}
	body, err := render.ParseWithClock("{{ advance }}{{ .Subject }}\n{{ .Body }}", text.FormatPlain, template.FuncMap{"advance": advanceOnce(&isTextAdvanced)}, budget, now)
	if err != nil {
		t.Fatal(err)
	}
	builtin, err := render.Builtin(text.FormatPlain)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("T-TPL-06/budget-shared-with-request-parts", func(t *testing.T) {
		sender := &recordingSender{caps: backend.Caps{MaxText: 100}}
		target := delivery.Target{
			ID: "t", Sender: sender, Template: body, Fallback: builtin, OnLong: delivery.OnLongTruncate,
			Request: &delivery.RequestTemplates{Headers: map[string]*render.Template{"X-H": header}},
		}
		d := render.Data{Subject: "s", Body: strings.Repeat("word ", 100), Strings: render.DefaultStrings()}
		result := delivery.Deliver(context.Background(), []delivery.Target{target}, d, nil)[0]
		if result.Status != delivery.OK || !isPartAdvanced.Load() || !isTextAdvanced.Load() || result.TemplateErr == nil ||
			!strings.Contains(result.TemplateErr.Error(), "budget of the text spent") {
			t.Fatalf("result = %+v", result)
		}
		if len(sender.texts) != 1 || strings.HasPrefix(sender.texts[0], "s\nword") {
			t.Errorf("texts = %q, want the built-in text", sender.texts)
		}
	})
}
