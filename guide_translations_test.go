package main

import (
	"os"
	"path"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// relativeLink matches the target of a markdown link, without its anchor.
var relativeLink = regexp.MustCompile(`\]\(([^)\s#]+)(?:#[^)\s]*)?\)`)

// guideLines reads a page as lines, without the empty one after the last
// newline.
func guideLines(t *testing.T, page string) []string {
	t.Helper()
	data, err := os.ReadFile(page)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

// headingLevels lists the level of every heading outside code blocks, where
// a line starting with # is a comment of the example.
func headingLevels(lines []string) []int {
	var levels []int
	inCode := false
	for _, line := range lines {
		if strings.HasPrefix(line, "```") {
			inCode = !inCode
			continue
		}
		if inCode {
			continue
		}
		level := len(line) - len(strings.TrimLeft(line, "#"))
		if level > 0 && strings.HasPrefix(line[level:], " ") {
			levels = append(levels, level)
		}
	}
	return levels
}

// TestGuideTranslations pins the pairing of every page of userGuides with
// its Russian translation: each ends with a link to the other, both have the
// same headings in the same order, and the links of a page to other pages
// stay in its language, so that a reader of the translation does not land
// on an English page or the other way round.
func TestGuideTranslations(t *testing.T) {
	for _, page := range userGuides {
		translation := translationOf(page)
		t.Run(page, func(t *testing.T) {
			english := guideLines(t, page)
			russian := guideLines(t, translation)
			footers := map[string]string{
				page:        "Russian: [" + path.Base(translation) + "](" + path.Base(translation) + ").",
				translation: "English: [" + path.Base(page) + "](" + path.Base(page) + ").",
			}
			for name, lines := range map[string][]string{page: english, translation: russian} {
				if last := lines[len(lines)-1]; last != footers[name] {
					t.Errorf("%s ends with %q, want %q", name, last, footers[name])
				}
				isTranslation := name == translation
				for i, line := range lines[:len(lines)-1] {
					for _, match := range relativeLink.FindAllStringSubmatch(line, -1) {
						target := match[1]
						if strings.Contains(target, "://") || !strings.HasSuffix(target, ".md") {
							continue
						}
						if strings.HasSuffix(target, ".ru.md") != isTranslation {
							t.Errorf("%s:%d links to %s, a page of the other language", name, i+1, target)
						}
					}
				}
			}
			if got, want := headingLevels(russian), headingLevels(english); !slices.Equal(got, want) {
				t.Errorf("%s has headings of levels %v, %s %v", translation, got, page, want)
			}
		})
	}
}
