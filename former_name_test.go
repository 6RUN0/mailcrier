package main

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// formerName is the name of the program before mailcrier, split so that
// this file does not carry it.
const formerName = "slend" + "mail"

// forkOrigin is the only text of the tree allowed to carry formerName: once,
// as a whole line of README.md.
const forkOrigin = "Started as a fork of github.com/iggy/" + formerName + "."

// TestFormerNameGone fails when the path or the content of a tracked file
// carries the former name of the program in any case, except forkOrigin in
// README.md. Without a git work tree it is skipped, unless CI is set: a
// source archive has no index to list, CI must not pass without checking.
func TestFormerNameGone(t *testing.T) {
	out, err := exec.Command("git", "rev-parse", "--is-inside-work-tree").Output()
	if err != nil || strings.TrimSpace(string(out)) != "true" {
		if os.Getenv("CI") != "" {
			t.Fatalf("not a git work tree (%v), CI must check the tracked files", err)
		}
		t.Skipf("not a git work tree (%v), no tracked files to check", err)
	}
	out, err = exec.Command("git", "ls-files", "-z").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	files := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
	for _, want := range []string{"go.mod", "former_name_test.go"} {
		if !slices.Contains(files, want) {
			t.Fatalf("git ls-files lacks %s: it does not list this tree", want)
		}
	}
	for _, file := range files {
		if strings.Contains(strings.ToLower(file), formerName) {
			t.Errorf("path %s carries the former name", file)
		}
		content, err := os.ReadFile(file)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		origins := 0
		for i, line := range strings.Split(string(content), "\n") {
			if file == "README.md" && line == forkOrigin {
				origins++
				continue
			}
			if strings.Contains(strings.ToLower(line), formerName) {
				t.Errorf("%s:%d carries the former name", file, i+1)
				break
			}
		}
		if file == "README.md" && origins != 1 {
			t.Errorf("README.md has the line %q %d times, want once", forkOrigin, origins)
		}
	}
}
