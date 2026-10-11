package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// source is internal/x/x.go of the fixtures; the mutant sits on the
// comparison of the return line.
const source = "package x\n\nfunc f(a int) bool {\n\treturn a > 0\n}\n"

// listed is the list entry of that mutant.
const listed = "internal/x/x.go\tCONDITIONALS_BOUNDARY\t9\treturn a > 0\tboth sides of 0 behave alike\n"

// fixture writes the source, a report with one mutant of the given status
// at line and column, and the list, and returns the root and the paths.
func fixture(t *testing.T, src, status string, line, column int, allow string) (root, allowPath, run string) {
	t.Helper()
	root = t.TempDir()
	dir := filepath.Join(root, "internal", "x")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "x.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	r := map[string]any{"files": []any{map[string]any{
		"file_name": "x.go",
		"mutations": []any{map[string]any{"type": "CONDITIONALS_BOUNDARY", "status": status, "line": line, "column": column}},
	}}}
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	reportPath := filepath.Join(root, "report.json")
	if err := os.WriteFile(reportPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	allowPath = filepath.Join(root, "allow.txt")
	if err := os.WriteFile(allowPath, []byte(allow), 0o644); err != nil {
		t.Fatal(err)
	}
	return root, allowPath, "internal/x=" + reportPath
}

// TestCheck pins which runs and lists give findings: a surviving mutant
// needs an entry, an entry needs a mutant that still survives, and a key
// survives edits that only move or reindent its line.
func TestCheck(t *testing.T) {
	tests := []struct {
		name   string
		src    string
		status string
		line   int
		column int
		allow  string
		want   string // substring of the only finding, empty for none
	}{
		{"new survivor", source, "LIVED", 4, 11, "", "kill it with a test or list it"},
		{"not covered", source, "NOT COVERED", 4, 11, "", "not covered, kill it"},
		{"listed survivor", source, "LIVED", 4, 11, listed, ""},
		{"listed and killed", source, "KILLED", 4, 11, listed, "killed now, remove the entry"},
		{"listed and timed out", source, "TIMED OUT", 4, 11, listed, ""},
		{"line moved", "package x\n\n// f.\nfunc f(a int) bool {\n\treturn a > 0\n}\n", "LIVED", 5, 11, listed, ""},
		{"line reindented", "package x\n\nfunc f(a int) bool {\n\t\treturn a > 0\n}\n", "LIVED", 4, 12, listed, ""},
		{"line changed", "package x\n\nfunc f(a int) bool {\n\treturn a >= 0\n}\n", "KILLED", 4, 11, listed, "no such mutant any more"},
		{"other directory", source, "KILLED", 4, 11, "internal/y/y.go\tCONDITIONALS_BOUNDARY\t9\treturn a > 0\tnot run\n", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root, allowPath, run := fixture(t, tt.src, tt.status, tt.line, tt.column, tt.allow)
			findings, err := check(root, allowPath, []string{run})
			if err != nil {
				t.Fatal(err)
			}
			if tt.want == "" {
				if len(findings) != 0 {
					t.Errorf("findings %q, want none", findings)
				}
				return
			}
			if len(findings) != 1 || !strings.Contains(findings[0], tt.want) {
				t.Errorf("findings %q, want one with %q", findings, tt.want)
			}
		})
	}
}

// TestCheckRejectsBadList pins that an entry without a reason or listed
// twice is an error of the check, not a finding.
func TestCheckRejectsBadList(t *testing.T) {
	for name, allow := range map[string]string{
		"no reason": "internal/x/x.go\tCONDITIONALS_BOUNDARY\t9\treturn a > 0\t \n",
		"twice":     listed + listed,
		"offset":    "internal/x/x.go\tCONDITIONALS_BOUNDARY\tnine\treturn a > 0\twhy\n",
	} {
		t.Run(name, func(t *testing.T) {
			root, allowPath, run := fixture(t, source, "LIVED", 4, 11, allow)
			if _, err := check(root, allowPath, []string{run}); err == nil {
				t.Error("check() error = nil")
			}
		})
	}
}
