package main

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const modulePath = "github.com/6RUN0/mailcrier"

// allowedImports lists, per package directory, the module packages its
// non-test files may import. A trailing "/*" matches every direct
// subpackage. The graph keeps leaves (text, message, redact) free of dependencies,
// keeps config ignorant of targets and templates, and leaves wiring to app
// alone. A package missing from the table fails the test, so a new package
// needs an explicit decision here.
var allowedImports = map[string][]string{
	"cmd/slendmail":      {"internal/app"},
	"internal/app":       {"internal/sendmail", "internal/config", "internal/message", "internal/route", "internal/render", "internal/text", "internal/delivery", "internal/spool", "internal/backend", "internal/backend/*", "internal/redact"},
	"internal/delivery":  {"internal/backend", "internal/render", "internal/text", "internal/message", "internal/spool"},
	"internal/backend/*": {"internal/backend", "internal/text", "internal/message"},
	"internal/backend":   {"internal/text", "internal/message"},
	"internal/render":    {"internal/text", "internal/message"},
	"internal/route":     {"internal/message"},
	"internal/spool":     {"internal/message"},
	"internal/sendmail":  {"internal/message"},
	"internal/config":    {},
	"internal/text":      {},
	"internal/message":   {},
	"internal/golden":    {},
	"internal/redact":    {},
	"scripts/*":          {},
}

func TestImportGraph(t *testing.T) {
	imports := map[string]map[string]bool{}
	for _, root := range []string{"cmd", "internal", "scripts"} {
		err := filepath.WalkDir(root, func(file string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() && entry.Name() == "testdata" {
				return filepath.SkipDir
			}
			if entry.IsDir() || !strings.HasSuffix(file, ".go") || strings.HasSuffix(file, "_test.go") {
				return nil
			}
			parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.ImportsOnly)
			if err != nil {
				return err
			}
			dir := filepath.ToSlash(filepath.Dir(file))
			if imports[dir] == nil {
				imports[dir] = map[string]bool{}
			}
			for _, spec := range parsed.Imports {
				imported, err := strconv.Unquote(spec.Path.Value)
				if err != nil {
					return err
				}
				if rest, ok := strings.CutPrefix(imported, modulePath+"/"); ok {
					imports[dir][rest] = true
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, dir := range sortedKeys(imports) {
		allowed, ok := rulesFor(dir)
		if !ok {
			t.Errorf("package %s has no entry in allowedImports", dir)
			continue
		}
		for _, imported := range sortedKeys(imports[dir]) {
			if !matchesAny(imported, allowed) {
				t.Errorf("forbidden import: %s -> %s", dir, imported)
			}
		}
	}
}

func rulesFor(dir string) ([]string, bool) {
	if allowed, ok := allowedImports[dir]; ok {
		return allowed, true
	}
	allowed, ok := allowedImports[path.Dir(dir)+"/*"]
	return allowed, ok
}

func matchesAny(imported string, patterns []string) bool {
	return slices.ContainsFunc(patterns, func(pattern string) bool {
		if parent, ok := strings.CutSuffix(pattern, "/*"); ok {
			return path.Dir(imported) == parent
		}
		return imported == pattern
	})
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}
