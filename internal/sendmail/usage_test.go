package sendmail

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// TestUsageListsOptions pins that --help names every long option and every
// short flag the parser knows, and the mode spellings and the variable
// that are not single entries of those tables, each as a word of its own:
// a flag added to the parser without --help fails here.
func TestUsageListsOptions(t *testing.T) {
	words := []string{OptionConfig, "-oi", "-bm", "-bi", "-bp", "-bs", "mailq", "newaliases", "MAILCRIER_CONFIG"}
	for option := range longModes {
		words = append(words, option)
	}
	for letter := range shortFlags {
		words = append(words, "-"+string(letter))
	}
	for _, word := range words {
		if !regexp.MustCompile(`(^|[\s,])` + regexp.QuoteMeta(word) + `([\s,.;=]|$)`).MatchString(Usage) {
			t.Errorf("Usage lacks %s", word)
		}
	}
}

// TestUsageListsIgnoredFlags pins that the line "Accepted and ignored:" of
// --help names exactly the short flags of the parser that have no effect:
// TestUsageListsOptions passes when a flag that acts, such as -t, is
// listed there too, or when an ignored one is only mentioned elsewhere.
func TestUsageListsIgnoredFlags(t *testing.T) {
	// The letters applyFlag and applyValue act on; -o counts as ignored,
	// -oi has a line of its own.
	const acting = "frFtibqI"
	var want []string
	for letter := range shortFlags {
		if !strings.ContainsRune(acting, rune(letter)) {
			want = append(want, "-"+string(letter))
		}
	}
	_, list, isFound := strings.Cut(Usage, "Accepted and ignored:")
	if !isFound {
		t.Fatal("Usage lacks the line Accepted and ignored")
	}
	list, _, _ = strings.Cut(list, ".")
	got := strings.Fields(list)
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("ignored flags of Usage = %v, parser = %v", got, want)
	}
}

// TestUsageListsExitStatuses pins that the exit status section of --help
// has exactly the codes of EXIT STATUS in the manual page, each starting
// a line, so that a code added to or dropped from the manual fails here.
func TestUsageListsExitStatuses(t *testing.T) {
	page, err := os.ReadFile("../../docs/mailcrier.8")
	if err != nil {
		t.Fatal(err)
	}
	_, man, isFound := strings.Cut(string(page), "\n.Sh EXIT STATUS\n")
	if !isFound {
		t.Fatal("manual page lacks EXIT STATUS")
	}
	man, _, _ = strings.Cut(man, "\n.Sh ")
	var want []string
	for _, match := range regexp.MustCompile(`(?m)^\.It (\d+)$`).FindAllStringSubmatch(man, -1) {
		want = append(want, match[1])
	}
	_, section, isFound := strings.Cut(Usage, "\nExit status:\n")
	if !isFound {
		t.Fatal("Usage lacks the section Exit status")
	}
	section, _, _ = strings.Cut(section, "\n\n")
	var got []string
	for _, match := range regexp.MustCompile(`(?m)^  (\d+) `).FindAllStringSubmatch(section, -1) {
		got = append(got, match[1])
	}
	if len(want) == 0 || !slices.Equal(got, want) {
		t.Errorf("exit statuses of Usage = %v, manual page = %v", got, want)
	}
}

// TestUsageLayout pins that --help fits an 80-column terminal without
// wrapping and prints the same everywhere: lines of at most 79 printable
// ASCII characters, no tabs, a final line break.
func TestUsageLayout(t *testing.T) {
	if !strings.HasSuffix(Usage, "\n") {
		t.Error("Usage does not end with a line break")
	}
	for i, line := range strings.Split(strings.TrimSuffix(Usage, "\n"), "\n") {
		if len(line) > 79 {
			t.Errorf("line %d has %d characters, more than 79: %q", i+1, len(line), line)
		}
		for _, c := range []byte(line) {
			if c < ' ' || c > '~' {
				t.Errorf("line %d has the byte %#x, not printable ASCII: %q", i+1, c, line)
				break
			}
		}
	}
}
