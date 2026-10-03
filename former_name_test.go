package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
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

// errNotOwnTree marks a directory that is not the top of a git work tree.
var errNotOwnTree = errors.New("not the top of a git work tree")

// trackedFiles lists the files git tracks in dir, relative to it. It gives
// errNotOwnTree when dir is not the top of a work tree: a source archive, or
// one unpacked inside the work tree of another repository, as distribution
// build recipes do, where git would list the files of that repository.
func trackedFiles(dir string) ([]string, error) {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errNotOwnTree, err)
	}
	top, err := filepath.EvalSymlinks(strings.TrimSpace(string(out)))
	if err != nil {
		return nil, err
	}
	own, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, err
	}
	if top != own {
		return nil, fmt.Errorf("%w: the work tree is %s", errNotOwnTree, top)
	}
	out, err = exec.Command("git", "-C", dir, "ls-files", "-z").Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-files: %w", err)
	}
	return strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00"), nil
}

// TestFormerNameGone fails when the path or the content of a tracked file
// carries the former name of the program in any case, except forkOrigin in
// README.md. Outside its own git work tree it is skipped, unless CI is set: a
// source archive has no index to list, CI must not pass without checking.
func TestFormerNameGone(t *testing.T) {
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	files, err := trackedFiles(dir)
	if errors.Is(err, errNotOwnTree) {
		if os.Getenv("CI") != "" {
			t.Fatalf("%v, CI must check the tracked files", err)
		}
		t.Skipf("%v, no tracked files to check", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"go.mod", "former_name_test.go"} {
		if !slices.Contains(files, want) {
			t.Fatalf("git ls-files lacks %s: it does not list this tree", want)
		}
	}
	for _, file := range files {
		if strings.Contains(strings.ToLower(file), formerName) {
			t.Errorf("path %s carries the former name", file)
		}
		content, err := os.ReadFile(filepath.Join(dir, file))
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

// TestTrackedFilesOutsideOwnTree pins that a tree unpacked inside the work
// tree of another repository counts as no work tree, so TestFormerNameGone
// skips there instead of failing on the files of that repository.
func TestTrackedFilesOutsideOwnTree(t *testing.T) {
	outer := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", outer).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	inner := filepath.Join(outer, "src", "mailcrier")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inner, "go.mod"), []byte("module example.org/m\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := trackedFiles(inner); !errors.Is(err, errNotOwnTree) {
		t.Errorf("trackedFiles(inner) error = %v, want errNotOwnTree", err)
	}
	if _, err := trackedFiles(outer); err != nil {
		t.Errorf("trackedFiles(outer) error = %v, want nil", err)
	}
}
