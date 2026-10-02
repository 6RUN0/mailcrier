package config

import (
	"errors"
	"regexp"
	"strings"
)

// errGlobBackslash is the one syntax error of a glob.
var errGlobBackslash = errors.New("ends with a backslash that escapes nothing")

// compileGlob translates glob into an RE2 expression and compiles it. A *
// matches any run of characters, line breaks and slashes included, a ? one
// character, and a backslash makes the character after it literal; there
// are no classes. The glob matches the whole string, with case ignored by
// Unicode simple case folding, as (?i) ignores it.
func compileGlob(glob string) (*regexp.Regexp, error) {
	var expr strings.Builder
	expr.WriteString(`(?is)^`)
	isEscaped := false
	for _, r := range glob {
		switch {
		case isEscaped:
			expr.WriteString(regexp.QuoteMeta(string(r)))
			isEscaped = false
		case r == '\\':
			isEscaped = true
		case r == '*':
			expr.WriteString(`.*`)
		case r == '?':
			expr.WriteString(`.`)
		default:
			expr.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	if isEscaped {
		return nil, errGlobBackslash
	}
	expr.WriteString(`$`)
	return regexp.Compile(expr.String())
}
