// Package golden compares test output with golden files in txtar format:
// one file per fixture, one section per output. Run the tests with -update
// to rewrite the golden files from the current output.
//
// The format is the one of golang.org/x/tools/txtar: an optional comment,
// then sections introduced by a line "-- name --". Parsing it takes a few
// lines, so the module does not depend on x/tools for it.
package golden

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files from the current output")

// Archive is the content of one golden file.
type Archive struct {
	// Comment is the text before the first section.
	Comment string
	// Sections are the named outputs, in file order.
	Sections []Section
}

// Section is one named output.
type Section struct {
	// Name is the text between "-- " and " --".
	Name string
	// Data is the section content; Format ends it with a newline.
	Data []byte
}

// Parse splits txtar data into its comment and sections.
func Parse(data []byte) Archive {
	var archive Archive
	var current *Section
	var buf bytes.Buffer
	flush := func() {
		if current == nil {
			archive.Comment = buf.String()
		} else {
			current.Data = bytes.Clone(buf.Bytes())
			archive.Sections = append(archive.Sections, *current)
		}
		buf.Reset()
	}
	for _, line := range strings.SplitAfter(string(data), "\n") {
		if name, ok := sectionName(line); ok {
			flush()
			current = &Section{Name: name}
			continue
		}
		buf.WriteString(line)
	}
	flush()
	return archive
}

func sectionName(line string) (string, bool) {
	trimmed := strings.TrimSuffix(line, "\n")
	if !strings.HasPrefix(trimmed, "-- ") || !strings.HasSuffix(trimmed, " --") || len(trimmed) < len("-- x --") {
		return "", false
	}
	return strings.TrimSpace(trimmed[3 : len(trimmed)-3]), true
}

// Format serializes the archive; every section ends with a newline so that
// Parse(Format(a)) returns the same sections.
func Format(archive Archive) []byte {
	var buf bytes.Buffer
	buf.WriteString(withNewline(archive.Comment))
	for _, section := range archive.Sections {
		buf.WriteString("-- " + section.Name + " --\n")
		buf.WriteString(withNewline(string(section.Data)))
	}
	return buf.Bytes()
}

func withNewline(text string) string {
	if text == "" || strings.HasSuffix(text, "\n") {
		return text
	}
	return text + "\n"
}

// Check compares got with the golden file at path, or rewrites the file
// when the test binary runs with -update.
func Check(t testing.TB, path string, got Archive) {
	t.Helper()
	formatted := Format(got)
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, formatted, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden file (run the test with -update to create it): %v", err)
	}
	want := Parse(data)
	got = Parse(formatted)
	if got.Comment != want.Comment {
		t.Errorf("%s: comment differs\n--- got ---\n%s--- want ---\n%s", path, got.Comment, want.Comment)
	}
	if len(got.Sections) != len(want.Sections) {
		t.Errorf("%s: %d sections, want %d", path, len(got.Sections), len(want.Sections))
	}
	for i := 0; i < len(got.Sections) && i < len(want.Sections); i++ {
		g, w := got.Sections[i], want.Sections[i]
		if g.Name != w.Name {
			t.Errorf("%s: section %d is %q, want %q", path, i+1, g.Name, w.Name)
			continue
		}
		if !bytes.Equal(g.Data, w.Data) {
			t.Errorf("%s: section %q differs\n--- got ---\n%s--- want ---\n%s", path, g.Name, g.Data, w.Data)
		}
	}
}
