package main

import (
	"bufio"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const testCasesPath = "testdata/test-cases.tsv"

var (
	testCaseID       = regexp.MustCompile(`^T-(ADJ|MTA|CALL|ESC|LIM|TPL|PKG)-\d{2}$`)
	testCaseMention  = regexp.MustCompile(`\bT-(ADJ|MTA|CALL|ESC|LIM|TPL|PKG)-\d+\b`)
	testCaseColumns  = []string{"id", "priority", "status", "behaviour", "source"}
	testCasePriority = map[string]bool{"critical": true, "high": true, "low": true}
	testCaseStatus   = map[string]bool{"todo": true, "done": true}
)

type testCase struct {
	priority, status string
}

// TestIDsCovered fails when a critical test case marked done in
// testdata/test-cases.tsv has no subtest named "<ID>/<slug>", or when a
// string literal of a test file names an ID the registry does not know.
// Subtest names are the string literals starting the first argument of
// each Run call; any other literal, such as a row of a table that feeds
// Run, is checked against the registry only.
func TestIDsCovered(t *testing.T) {
	cases := readTestCases(t)
	covered, mentioned := collectRunIDs(t)
	for id, tc := range cases {
		if tc.priority == "critical" && tc.status == "done" && !covered[id] {
			t.Errorf("%s is critical and done but no t.Run(%q) exists", id, id+"/<slug>")
		}
	}
	for id, file := range mentioned {
		if _, ok := cases[id]; !ok {
			t.Errorf("%s names %s, which %s does not list", file, id, testCasesPath)
		}
	}
}

func readTestCases(t *testing.T) map[string]testCase {
	t.Helper()
	file, err := os.Open(testCasesPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	cases := map[string]testCase{}
	scanner := bufio.NewScanner(file)
	for line := 1; scanner.Scan(); line++ {
		fields := strings.Split(scanner.Text(), "\t")
		if line == 1 {
			if strings.Join(fields, "\t") != strings.Join(testCaseColumns, "\t") {
				t.Fatalf("%s:1: header %q, want %q", testCasesPath, fields, testCaseColumns)
			}
			continue
		}
		if len(fields) != len(testCaseColumns) {
			t.Fatalf("%s:%d: %d columns, want %d", testCasesPath, line, len(fields), len(testCaseColumns))
		}
		id, priority, status, behaviour, source := fields[0], fields[1], fields[2], fields[3], fields[4]
		switch {
		case !testCaseID.MatchString(id):
			t.Errorf("%s:%d: malformed id %q", testCasesPath, line, id)
		case cases[id] != (testCase{}):
			t.Errorf("%s:%d: duplicate id %s", testCasesPath, line, id)
		case !testCasePriority[priority]:
			t.Errorf("%s:%d: %s: unknown priority %q", testCasesPath, line, id, priority)
		case !testCaseStatus[status]:
			t.Errorf("%s:%d: %s: unknown status %q", testCasesPath, line, id, status)
		case behaviour == "":
			t.Errorf("%s:%d: %s: empty behaviour", testCasesPath, line, id)
		case !strings.HasPrefix(source, "https://"):
			t.Errorf("%s:%d: %s: source is not a public https link", testCasesPath, line, id)
		}
		cases[id] = testCase{priority: priority, status: status}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return cases
}

// collectRunIDs parses every test file of the module, including its own,
// and returns the test case IDs that name subtests, and every ID a string
// literal names with the first file that names it.
func collectRunIDs(t *testing.T) (covered map[string]bool, mentioned map[string]string) {
	t.Helper()
	covered, mentioned = map[string]bool{}, map[string]string{}
	err := filepath.WalkDir(".", func(file string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() && file != "." && (strings.HasPrefix(entry.Name(), ".") || entry.Name() == "testdata" || entry.Name() == "tools") {
			return filepath.SkipDir
		}
		if entry.IsDir() || !strings.HasSuffix(file, "_test.go") {
			return nil
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(parsed, func(node ast.Node) bool {
			if id, ok := runCaseID(node); ok {
				covered[id] = true
			}
			for _, id := range literalCaseIDs(node) {
				if _, ok := mentioned[id]; !ok {
					mentioned[id] = file
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return covered, mentioned
}

// literalCaseIDs returns the test case IDs that node names when it is a
// string literal.
func literalCaseIDs(node ast.Node) []string {
	literal, ok := node.(*ast.BasicLit)
	if !ok || literal.Kind != token.STRING {
		return nil
	}
	text, err := strconv.Unquote(literal.Value)
	if err != nil {
		return nil
	}
	return testCaseMention.FindAllString(text, -1)
}

// TestLiteralCaseIDs pins that an ID in a table row, outside any Run
// call, reaches the registry check, a typo included.
func TestLiteralCaseIDs(t *testing.T) {
	// The IDs are spelled with @ for T-, so that this file names none.
	src := strings.ReplaceAll("package p\nvar rows = []struct{ name string }{{\"@ADJ-06/strip\"}, {`@MTA-123 and @ESC-1`}, {\"X@ADJ-01\"}}\n", "@", "T-")
	parsed, err := parser.ParseFile(token.NewFileSet(), "rows.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	ast.Inspect(parsed, func(node ast.Node) bool {
		got = append(got, literalCaseIDs(node)...)
		return true
	})
	if want := strings.Fields(strings.ReplaceAll("@ADJ-06 @MTA-123 @ESC-1", "@", "T-")); !slices.Equal(got, want) {
		t.Errorf("literalCaseIDs = %v, want %v", got, want)
	}
}

// runCaseID returns the test case ID of a call X.Run("T-.../slug"...), where
// the first argument is a string literal or a concatenation starting with
// one.
func runCaseID(node ast.Node) (string, bool) {
	call, ok := node.(*ast.CallExpr)
	if !ok || len(call.Args) == 0 {
		return "", false
	}
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != "Run" {
		return "", false
	}
	first := call.Args[0]
	for {
		binary, ok := first.(*ast.BinaryExpr)
		if !ok || binary.Op != token.ADD {
			break
		}
		first = binary.X
	}
	literal, ok := first.(*ast.BasicLit)
	if !ok || literal.Kind != token.STRING {
		return "", false
	}
	name, err := strconv.Unquote(literal.Value)
	if err != nil {
		return "", false
	}
	id, _, _ := strings.Cut(name, "/")
	return id, testCaseID.MatchString(id)
}
