package sendmail

import (
	"os"
	"regexp"
	"testing"
)

// TestManPageListsOptions pins that the OPTIONS list of the manual page
// has an item for every long option and every short flag the parser
// knows: each one starts an .It line or follows it as an argument of its
// own to Fl, so that the synopsis, which names them too, does not count,
// and neither does ".Fl tiIv" for t.
func TestManPageListsOptions(t *testing.T) {
	page, err := os.ReadFile("../../docs/mailcrier.8")
	if err != nil {
		t.Fatal(err)
	}
	man := string(page)
	options := []string{OptionConfig}
	for option := range longModes {
		options = append(options, option)
	}
	for _, option := range options {
		if !regexp.MustCompile(`(?m)^\.It Fl ` + regexp.QuoteMeta(option[1:]) + `\b`).MatchString(man) {
			t.Errorf("OPTIONS of the manual page lack .It Fl %s", option[1:])
		}
	}
	for letter := range shortFlags {
		if !regexp.MustCompile(`(?m)^\.It .*\bFl ` + regexp.QuoteMeta(string(letter)) + `( |,|$)`).MatchString(man) {
			t.Errorf("OPTIONS of the manual page lack an item with Fl %c", letter)
		}
	}
}
