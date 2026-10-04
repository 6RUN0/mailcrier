package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// releaseEnv is the environment of a make run in these tests: without the
// variables of CI and of an outer make, so that a run inside GitHub Actions
// neither passes the guard nor sends its token to the API.
func releaseEnv(extra ...string) []string {
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		switch name {
		case "GITHUB_ACTIONS", "GH_TOKEN", "GITHUB_TOKEN", "GITHUB_REPOSITORY", "TAG",
			"MAKEFLAGS", "MFLAGS", "MAKELEVEL", "GIT_DIR", "GIT_WORK_TREE":
			continue
		}
		env = append(env, kv)
	}
	env = append(env,
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.org",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.org")
	return append(env, extra...)
}

func runIn(t *testing.T, dir string, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = releaseEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %q: %v\n%s", name, args, err, out)
	}
}

// TestReleaseGate runs the git part of make release-gate in a clone of a
// throwaway origin: each way a tag may sit on the wrong commit stops the
// gate with its own message before check-release, and a tag on a commit of
// main and develop reaches check-release, which without a token exits 2.
// A wrong gate would show only on a real tag, which cannot be deleted.
func TestReleaseGate(t *testing.T) {
	for _, tool := range []string{"git", "make"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		prepare func(t *testing.T, dir string)
		want    string
	}{
		{"passes to check-release", func(t *testing.T, dir string) {
			runIn(t, dir, "git", "tag", "-a", "v1.0.0", "-m", "v1.0.0")
		}, "check-release: GITHUB_REPOSITORY and GH_TOKEN must be set"},
		{"tag not on HEAD", func(t *testing.T, dir string) {
			runIn(t, dir, "git", "tag", "-a", "v1.0.0", "-m", "v1.0.0")
			runIn(t, dir, "git", "commit", "-q", "--allow-empty", "-m", "fix: later")
		}, "tag v1.0.0 does not point at HEAD"},
		{"missing tag", func(*testing.T, string) {}, "tag v1.0.0 does not point at HEAD"},
		{"main ahead of develop", func(t *testing.T, dir string) {
			runIn(t, dir, "git", "checkout", "-q", "main")
			runIn(t, dir, "git", "commit", "-q", "--allow-empty", "-m", "fix: on main only")
			runIn(t, dir, "git", "push", "-q", "origin", "main")
			runIn(t, dir, "git", "checkout", "-q", "develop")
			runIn(t, dir, "git", "tag", "-a", "v1.0.0", "-m", "v1.0.0")
		}, "main has commits that develop lacks"},
		{"HEAD not in main", func(t *testing.T, dir string) {
			runIn(t, dir, "git", "commit", "-q", "--allow-empty", "-m", "fix: on develop only")
			runIn(t, dir, "git", "push", "-q", "origin", "develop")
			runIn(t, dir, "git", "tag", "-a", "v1.0.0", "-m", "v1.0.0")
		}, "HEAD is not in main"},
		{"missing origin/main", func(t *testing.T, dir string) {
			runIn(t, dir, "git", "tag", "-a", "v1.0.0", "-m", "v1.0.0")
			runIn(t, dir, "git", "update-ref", "-d", "refs/remotes/origin/main")
		}, "origin/main or origin/develop is missing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			origin, dir := filepath.Join(base, "origin.git"), filepath.Join(base, "clone")
			runIn(t, base, "git", "init", "-q", "--bare", origin)
			runIn(t, base, "git", "clone", "-q", origin, dir)
			runIn(t, dir, "git", "checkout", "-q", "-b", "develop")
			runIn(t, dir, "git", "commit", "-q", "--allow-empty", "-m", "feat: first")
			runIn(t, dir, "git", "push", "-q", "origin", "develop", "develop:main")
			runIn(t, dir, "git", "fetch", "-q", "origin")
			tc.prepare(t, dir)
			// go -C runs check-release from this module while git runs in the clone.
			cmd := exec.Command("make", "-f", filepath.Join(root, "Makefile"), "release-gate", "GO=go -C "+root)
			cmd.Dir = dir
			cmd.Env = releaseEnv("TAG=v1.0.0")
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("release-gate passed:\n%s", out)
			}
			if !strings.Contains(string(out), tc.want) {
				t.Fatalf("release-gate output lacks %q:\n%s", tc.want, out)
			}
			if tc.name != "passes to check-release" && strings.Contains(string(out), "check-release") {
				t.Fatalf("release-gate reached check-release:\n%s", out)
			}
		})
	}
}

// TestReleaseGuard pins the refusals of make release before goreleaser:
// outside GitHub Actions, and for a TAG that is not a version, a newline
// included, since grep alone would accept a valid first line.
func TestReleaseGuard(t *testing.T) {
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make not installed")
	}
	cases := []struct {
		name string
		env  []string
		want string
	}{
		{"outside actions", []string{"TAG=v1.0.0"}, "release publishes to GitHub; run it from release.yml"},
		{"shell syntax", []string{"GITHUB_ACTIONS=true", "TAG=v1;x"}, "release needs TAG=vX.Y.Z[-pre]"},
		{"newline", []string{"GITHUB_ACTIONS=true", "TAG=v1.0.0\nv2"}, "release needs TAG=vX.Y.Z[-pre]"},
		{"empty", []string{"GITHUB_ACTIONS=true"}, "release needs TAG=vX.Y.Z[-pre]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command("make", "release-guard")
			cmd.Env = releaseEnv(tc.env...)
			out, err := cmd.CombinedOutput()
			if err == nil || !strings.Contains(string(out), tc.want) {
				t.Fatalf("make release-guard = %v, output lacks %q:\n%s", err, tc.want, out)
			}
		})
	}
	cmd := exec.Command("make", "release-guard")
	cmd.Env = releaseEnv("GITHUB_ACTIONS=true", "TAG=v1.0.0-rc.1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("make release-guard with a valid tag: %v\n%s", err, out)
	}
}
