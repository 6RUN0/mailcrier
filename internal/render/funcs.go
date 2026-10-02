package render

import (
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"

	"github.com/6RUN0/slendmail/internal/text"
)

// title capitalizes the first letter of every word by the rules common to
// all languages and leaves the other letters as they are, so that "SMART"
// in a subject keeps its capitals. A slash starts a word, as it does for
// cases.Title: "/dev/sda" becomes "/Dev/Sda".
func title(s string) string {
	return cases.Title(language.Und, cases.NoLower).String(s)
}

// join joins the elements of list, strings or values with a String
// method such as message.Address, with sep between them. sep comes first,
// so that a pipeline passes the list: {{ .To | join ", " }}.
func join(sep string, list any) (string, error) {
	if list == nil {
		return "", nil
	}
	if strs, ok := list.([]string); ok {
		return strings.Join(strs, sep), nil
	}
	value := reflect.ValueOf(list)
	if value.Kind() != reflect.Slice && value.Kind() != reflect.Array {
		return "", fmt.Errorf("join: want a list, got %T", list)
	}
	parts := make([]string, value.Len())
	for i := range parts {
		switch item := value.Index(i).Interface().(type) {
		case string:
			parts[i] = item
		case fmt.Stringer:
			parts[i] = item.String()
		default:
			return "", fmt.Errorf("join: cannot join an element of type %T", item)
		}
	}
	return strings.Join(parts, sep), nil
}

// maxCachedPatterns bounds the cache of compiled patterns: a pattern
// taken from the message would otherwise add one entry per message of a
// queue run.
const maxCachedPatterns = 64

// patterns caches the compiled patterns of match and reReplaceAll, which
// a template inside range would otherwise compile once per item.
var patterns = struct {
	sync.Mutex
	compiled map[string]*regexp.Regexp
}{compiled: map[string]*regexp.Regexp{}}

// compilePattern returns pattern compiled, from the cache when it is
// there.
func compilePattern(pattern string) (*regexp.Regexp, error) {
	patterns.Lock()
	defer patterns.Unlock()
	if re, ok := patterns.compiled[pattern]; ok {
		return re, nil
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, err
	}
	if len(patterns.compiled) < maxCachedPatterns {
		patterns.compiled[pattern] = re
	}
	return re, nil
}

// match reports whether s contains a match of pattern, a Go regular
// expression.
func match(pattern, s string) (bool, error) {
	re, err := compilePattern(pattern)
	if err != nil {
		return false, err
	}
	return re.MatchString(s), nil
}

// reReplaceAll replaces the matches of pattern in s with replacement, in
// which $1 and ${name} stand for submatches.
func reReplaceAll(pattern, replacement, s string) (string, error) {
	re, err := compilePattern(pattern)
	if err != nil {
		return "", err
	}
	return re.ReplaceAllString(s, replacement), nil
}

// date formats t by a Go layout such as "2006-01-02 15:04".
func date(layout string, t time.Time) string {
	return t.Format(layout)
}

// errUnknownZone leaves the zone name out: it may come from the message,
// and the error goes to the log.
var errUnknownZone = errors.New("unknown time zone")

// inZone, the function tz, returns t in the IANA zone name, such as
// "Europe/Berlin", "UTC" or "Local".
func inZone(name string, t time.Time) (time.Time, error) {
	zone, err := time.LoadLocation(name)
	if err != nil {
		return time.Time{}, errUnknownZone
	}
	return t.In(zone), nil
}

// lines splits s into lines; a line break at the end of s does not start
// another, empty line, and an empty s has no line.
func lines(s string) []string {
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// head returns the first n lines of s, joined by line breaks, without one
// at the end.
func head(n int, s string) string {
	all := lines(s)
	return strings.Join(all[:min(max(n, 0), len(all))], "\n")
}

// tail returns the last n lines of s, joined by line breaks, without one
// at the end.
func tail(n int, s string) string {
	all := lines(s)
	return strings.Join(all[len(all)-min(max(n, 0), len(all)):], "\n")
}

// truncateMark ends a string that truncate has cut.
const truncateMark = "..."

// truncate returns s cut to n characters, the last three of which are
// truncateMark; for n of 3 or less there is no room for the mark, and s
// is cut to n characters without it.
func truncate(n int, s string) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	if n <= len(truncateMark) {
		return text.TruncateRunes(s, n)
	}
	return text.TruncateRunes(s, n-len(truncateMark)) + truncateMark
}

// indent puts n spaces before every line of s, the first one included.
func indent(n int, s string) string {
	pad := strings.Repeat(" ", max(n, 0))
	return pad + strings.ReplaceAll(s, "\n", "\n"+pad)
}
