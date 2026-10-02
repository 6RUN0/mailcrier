package config

import (
	"errors"
	"regexp/syntax"
	"testing"
	"unicode"
)

// TestCompileGlob pins the glob syntax of the subject, sender and
// recipient conditions: whole-string match, case folded, * across line
// breaks and slashes, ? for one character, \ for a literal.
func TestCompileGlob(t *testing.T) {
	cases := []struct {
		glob, value string
		want        bool
	}{
		{"*raid*", "mdadm: RAID degraded on /dev/md0", true},
		{"raid", "raid1", false},
		{"*anacron*", "Anacron job 'cron.daily' on host", true},
		{"*/etc/*", "changed /etc/passwd", true},
		{"a*b", "a\nline\nb", true},
		{"?ron", "cron", true},
		{"?ron", "ron", false},
		{"ж?", "Жё", true},
		{"*ОШИБКА*", "сбой: ошибка диска", true},
		{"*k*", "\u212a", true}, // the Kelvin sign folds to k
		{`\*`, "*", true},
		{`\*`, "x", false},
		{`\?`, "?", true},
		{`a\\b`, `a\b`, true},
		{`\a`, "a", true},
		{"[ab]", "[ab]", true},
		{"[ab]", "a", false},
		{"a.c", "abc", false},
		{"(x)+", "(x)+", true},
		{"", "", true},
		{"", "x", false},
	}
	for _, tc := range cases {
		re, err := compileGlob(tc.glob)
		if err != nil {
			t.Errorf("compileGlob(%q): %v", tc.glob, err)
			continue
		}
		if got := re.MatchString(tc.value); got != tc.want {
			t.Errorf("glob %q on %q = %v, want %v", tc.glob, tc.value, got, tc.want)
		}
	}
	for _, glob := range []string{`\`, `abc\`, `a\\\`} {
		if _, err := compileGlob(glob); !errors.Is(err, errGlobBackslash) {
			t.Errorf("compileGlob(%q) error = %v, want %v", glob, err, errGlobBackslash)
		}
	}
}

// FuzzGlob compares the RE2 translation of a glob with a direct match over
// runes.
func FuzzGlob(f *testing.F) {
	for _, seed := range [][2]string{
		{"*raid*", "RAID degraded"}, {`a\*?b`, "a*xb"}, {"ж*Ё", "Жabcё"}, {`\`, ""}, {"?*?", "\xff\n"}, {"[a-z]", "[a-z]"},
	} {
		f.Add(seed[0], seed[1])
	}
	f.Fuzz(func(t *testing.T, glob, value string) {
		re, err := compileGlob(glob)
		var syntaxErr *syntax.Error
		switch {
		case errors.As(err, &syntaxErr) && syntaxErr.Code == syntax.ErrLarge:
			return
		case err != nil:
			if want, ok := matchGlobRunes(glob, value); ok {
				t.Fatalf("compileGlob(%q): %v; the direct match gives %v", glob, err, want)
			}
			return
		}
		want, ok := matchGlobRunes(glob, value)
		if !ok {
			t.Fatalf("compileGlob(%q) succeeded, the direct match finds a trailing backslash", glob)
		}
		if got := re.MatchString(value); got != want {
			t.Fatalf("glob %q on %q: regexp %v, direct %v", glob, value, got, want)
		}
	})
}

// globToken is one element of a glob in matchGlobRunes: a literal rune,
// or anyOne for ?, or anyRun for *.
type globToken struct {
	literal        rune
	anyOne, anyRun bool
}

// matchGlobRunes matches value against glob by dynamic programming over
// runes; ok is false for a trailing backslash.
func matchGlobRunes(glob, value string) (matched, ok bool) {
	var tokens []globToken
	isEscaped := false
	for _, r := range glob {
		switch {
		case isEscaped:
			tokens, isEscaped = append(tokens, globToken{literal: r}), false
		case r == '\\':
			isEscaped = true
		case r == '*':
			tokens = append(tokens, globToken{anyRun: true})
		case r == '?':
			tokens = append(tokens, globToken{anyOne: true})
		default:
			tokens = append(tokens, globToken{literal: r})
		}
	}
	if isEscaped {
		return false, false
	}
	runes := []rune(value)
	// matches[j] reports whether the tokens so far match runes[:j].
	matches := make([]bool, len(runes)+1)
	matches[0] = true
	for _, token := range tokens {
		next := make([]bool, len(runes)+1)
		for j := range next {
			switch {
			case token.anyRun:
				next[j] = matches[j] || (j > 0 && next[j-1])
			case j == 0:
			case token.anyOne:
				next[j] = matches[j-1]
			default:
				next[j] = matches[j-1] && equalFold(token.literal, runes[j-1])
			}
		}
		matches = next
	}
	return matches[len(runes)], true
}

// equalFold reports whether a and b are in one orbit of Unicode simple
// case folding.
func equalFold(a, b rune) bool {
	for r := a; ; {
		if r == b {
			return true
		}
		if r = unicode.SimpleFold(r); r == a {
			return false
		}
	}
}

// TestGlobErrorText pins that an error of the translated expression is
// reported by its code, never by its text, which quotes the glob.
func TestGlobErrorText(t *testing.T) {
	if got := globErrorText(errGlobBackslash); got != errGlobBackslash.Error() {
		t.Errorf("globErrorText(backslash) = %q", got)
	}
	err := &syntax.Error{Code: syntax.ErrLarge, Expr: "MARKER"}
	if got := globErrorText(err); got != "is not a valid glob: expression too large" {
		t.Errorf("globErrorText(%v) = %q", err, got)
	}
}
