package config

import (
	"strings"
	"testing"
)

// TestIndexKeysArrayTables pins that every element of an array of tables
// gets its own key paths, with the index after the name of the array, so
// that an error in the third element points at the third.
func TestIndexKeysArrayTables(t *testing.T) {
	doc := strings.Join([]string{
		"[[route]]",           // 1
		"targets = [\"a\"]",   // 2
		"",                    // 3
		"[[route]]",           // 4
		"subject = \"x\"",     // 5
		"targets = [\"b\"]",   // 6
		"[[route]]",           // 7
		"  targets = [\"c\"]", // 8
		"[[outer]]",           // 9
		"[[outer.inner]]",     // 10
		"key = 1",             // 11
		"[[outer]]",           // 12
		"[[outer.inner]]",     // 13
		"[[outer.inner]]",     // 14
		"key = 2",             // 15
		"[outer.table]",       // 16
		"name = 3",            // 17
	}, "\n")
	keys, err := indexKeys([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path      []string
		line, col int
	}{
		{[]string{"route"}, 1, 3},
		{elementPath("route", 0), 1, 3},
		{elementPath("route", 0, "targets"), 2, 1},
		{elementPath("route", 1, "targets"), 6, 1},
		{elementPath("route", 1, "subject"), 5, 1},
		{elementPath("route", 2, "targets"), 8, 3},
		{elementPath("route", 2), 7, 3},
		{append(elementPath("outer", 0), "inner", "0", "key"), 11, 1},
		{append(elementPath("outer", 1), "inner", "1", "key"), 15, 1},
		{append(elementPath("outer", 1), "table", "name"), 17, 1},
	} {
		if !keys.has(tc.path...) {
			t.Errorf("%q not indexed", strings.Join(tc.path, "."))
			continue
		}
		if pos := keys.position(tc.path...); pos.Line != tc.line || pos.Column != tc.col {
			t.Errorf("%q at %d:%d, want %d:%d", strings.Join(tc.path, "."), pos.Line, pos.Column, tc.line, tc.col)
		}
	}
	if keys.has(elementPath("route", 0, "subject")...) {
		t.Error("subject of the second element indexed in the first")
	}
	if pos := keys.position(elementPath("route", 2, "subject")...); pos.Line != 7 {
		t.Errorf("absent key of the third element at line %d, want the header at 7", pos.Line)
	}
}
