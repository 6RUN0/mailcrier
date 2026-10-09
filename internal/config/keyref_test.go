package config

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// keyRow is a row of the table "Target keys" of docs/configuration.md:
// the key, its types and the types that require it.
var keyRow = regexp.MustCompile("(?m)^\\| `([a-z_]+)` \\|([^|]*)\\|([^|]*)\\|")

// codeNames returns the names in backticks of a table cell, sorted.
func codeNames(cell string) []string {
	var names []string
	for _, match := range regexp.MustCompile("`([a-z]+)`").FindAllStringSubmatch(cell, -1) {
		names = append(names, match[1])
	}
	slices.Sort(names)
	return names
}

// TestKeyReferenceMatchesSchema pins the table "Target keys" of
// docs/configuration.md to allowedKeys and requiredKeys both ways, so that
// a new key or type cannot leave the reference behind.
func TestKeyReferenceMatchesSchema(t *testing.T) {
	doc, err := os.ReadFile("../../docs/configuration.md")
	if err != nil {
		t.Fatal(err)
	}
	_, section, found := strings.Cut(string(doc), "\n## Target keys\n")
	if !found {
		t.Fatal("docs/configuration.md has no section Target keys")
	}
	section, _, _ = strings.Cut(section, "\n## ")
	wantTypes := map[string][]string{}
	for typ, keys := range allowedKeys {
		for _, key := range keys {
			wantTypes[key] = append(wantTypes[key], typ)
		}
	}
	wantRequired := map[string][]string{}
	for typ, keys := range requiredKeys {
		for _, key := range keys {
			wantRequired[key] = append(wantRequired[key], typ)
		}
	}
	listed := map[string]bool{}
	for _, row := range keyRow.FindAllStringSubmatch(section, -1) {
		key := row[1]
		if key == "type" {
			continue
		}
		listed[key] = true
		want := wantTypes[key]
		slices.Sort(want)
		if got := codeNames(row[2]); !slices.Equal(got, want) {
			t.Errorf("key %q: types %v, want %v", key, got, want)
		}
		want = wantRequired[key]
		slices.Sort(want)
		if got := codeNames(row[3]); !slices.Equal(got, want) {
			t.Errorf("key %q: required by %v, want %v", key, got, want)
		}
	}
	for key := range wantTypes {
		if !listed[key] {
			t.Errorf("key %q has no row", key)
		}
	}
}
