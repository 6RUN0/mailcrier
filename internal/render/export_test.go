package render

import (
	"text/template"
	"time"

	"github.com/6RUN0/mailcrier/internal/text"
)

// ParseWithClock is Parse with extra functions, a budget and the clock
// of the budget, for the tests of package render_test, which drive a
// template through delivery.
func ParseWithClock(source string, format text.Format, extra template.FuncMap, budget time.Duration, now func() time.Time) (*Template, error) {
	tmpl, err := parseUser("custom", source, format, extra)
	if err != nil {
		return nil, err
	}
	tmpl.user.budget, tmpl.user.now = budget, now
	return tmpl, nil
}

// ParsePartWithClock is ParsePart with extra functions, a budget and the
// clock of the budget.
func ParsePartWithClock(source string, extra template.FuncMap, budget time.Duration, now func() time.Time) (*Template, error) {
	tmpl, err := ParseWithClock(source, text.FormatPlain, extra, budget, now)
	if err != nil {
		return nil, err
	}
	tmpl.mayBeEmpty = true
	return tmpl, nil
}
