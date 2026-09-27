package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
)

func TestFindRefsInCommitMessage(t *testing.T) {
	pass := []string{
		"build: compile with -O1",
		"fix: pin sha256 of the base image",
		"test: cover T-MTA-25 glued values",
		"docs: mark the case critical",
		"feat: add X-Cron-Env to template data",
		"fix: handle UTF-16 length",
		"docs: describe backstage 5 lines",
	}
	for _, msg := range pass {
		if got := findRefs("msg", msg, commentText); len(got) != 0 {
			t.Errorf("findRefs(%q) = %v, want none", msg, got)
		}
	}
	fail := map[string]string{
		"fix per R3-M2":             "R3-M2",
		"stage 5":                   "stage 5",
		"see P7":                    "P7",
		"implement O1":              "O1",
		"as agreed in U-4":          "U-4",
		"row X-09 of the matrix":    "X-09",
		"architect A-B2 said so":    "A-B2",
		"critic C-m3":               "C-m3",
		"как решено на этапе 3":     "этапе 3",
		"see N-B1 and review A-15":  "N-B1",
		"Stage 2: sendmail parsing": "Stage 2",
		"(P0)":                      "P0",
	}
	for msg, want := range fail {
		got := findRefs("msg", msg, commentText)
		if len(got) == 0 {
			t.Errorf("findRefs(%q) found nothing", msg)
			continue
		}
		if want != "" && got[0].ref != want {
			t.Errorf("findRefs(%q)[0] = %q, want %q", msg, got[0].ref, want)
		}
	}
}

func TestFindRefsInDocumentAllowsDecisionLikeTokens(t *testing.T) {
	if got := findRefs("README.md", "Press P7 or O2 to continue", documentText); len(got) != 0 {
		t.Errorf("findRefs() = %v, want none in documentation", got)
	}
	if got := findRefs("README.md", "line one\nsee stage 4\n", documentText); len(got) != 1 || got[0].line != 2 {
		t.Errorf("findRefs() = %v, want one finding on line 2", got)
	}
}

func TestCheckFile(t *testing.T) {
	cases := []struct {
		name    string
		file    string
		content string
		want    []finding
	}{
		{
			name:    "go-comment",
			file:    "x.go",
			content: "package x\n\n// Load parses the file.\n// Strict since P15.\nvar s = \"P15 in a string is data\"\n",
			want:    []finding{{where: "x.go", line: 4, ref: "P15"}},
		},
		{
			name:    "go-block-comment",
			file:    "x.go",
			content: "package x\n\n/*\nfirst\nsee U-2\n*/\n",
			want:    []finding{{where: "x.go", line: 5, ref: "U-2"}},
		},
		{
			name:    "yaml-comment",
			file:    ".github/workflows/ci.yml",
			content: "on: push\njobs: {} # added in stage 0\nname: P7\n",
			want:    []finding{{where: ".github/workflows/ci.yml", line: 2, ref: "stage 0"}},
		},
		{
			name:    "makefile-comment",
			file:    "Makefile",
			content: "# per R5-m3\nlint:\n",
			want:    []finding{{where: "Makefile", line: 1, ref: "R5-m3"}},
		},
		{
			name:    "markdown",
			file:    "README.md",
			content: "# Title\n\nDecided in N-M4.\n",
			want:    []finding{{where: "README.md", line: 3, ref: "N-M4"}},
		},
		{
			name:    "man-page",
			file:    "docs/slendmail.8",
			content: ".TH SLENDMAIL 8\nsee X-12\n",
			want:    []finding{{where: "docs/slendmail.8", line: 2, ref: "X-12"}},
		},
		{
			name:    "fixture-is-data",
			file:    "testdata/callers/cronie.eml",
			content: "Subject: stage 5 of backup\n",
			want:    nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := checkFile(tc.file, []byte(tc.content))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("checkFile() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCheckedMessageKeepsOnlyDependabotSubject(t *testing.T) {
	body := "build(deps): bump x from 1 to 2\n\nRelease notes: fixed in stage 2, see P7.\n"
	if got, want := checkedMessage(dependabotEmail, body), "build(deps): bump x from 1 to 2"; got != want {
		t.Errorf("checkedMessage(dependabot) = %q, want %q", got, want)
	}
	if got := checkedMessage("jane@example.org", body); got != body {
		t.Errorf("checkedMessage(person) = %q, want the whole message", got)
	}
}

// TestCheckReadsFileNamesWithSpaces pins that changed and untracked file
// names with spaces and non-ASCII bytes reach the check unquoted.
func TestCheckReadsFileNamesWithSpaces(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	for _, args := range [][]string{
		{"init", "-q"},
		{"-c", "user.name=t", "-c", "user.email=t@example.org", "commit", "-q", "--allow-empty", "-m", "chore: start"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	name := "notes a b ё.md"
	if err := os.WriteFile(filepath.Join(dir, name), []byte("see stage 3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	findings, err := check("HEAD")
	if err != nil {
		t.Fatalf("check() error = %v", err)
	}
	want := []finding{{where: name, line: 1, ref: "stage 3"}}
	if !reflect.DeepEqual(findings, want) {
		t.Errorf("check() = %v, want %v", findings, want)
	}
}
