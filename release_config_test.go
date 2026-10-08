package main

import (
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// TestReleaseConfig pins the release section of .goreleaser.yaml: make
// release publishes at once, an rc tag becomes a prerelease, a rerun of the
// publish job after the release went public replaces its files instead of
// failing on the first one that exists, and the checksum file keeps the
// name release.yml attests through.
func TestReleaseConfig(t *testing.T) {
	data, err := os.ReadFile(".goreleaser.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Release struct {
			Disable    any    `yaml:"disable"`
			Draft      *bool  `yaml:"draft"`
			Prerelease string `yaml:"prerelease"`
			Replace    bool   `yaml:"replace_existing_artifacts"`
		} `yaml:"release"`
		Changelog struct {
			Disable any `yaml:"disable"`
		} `yaml:"changelog"`
		Checksum struct {
			NameTemplate string `yaml:"name_template"`
		} `yaml:"checksum"`
	}
	if err := yaml.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	if isTrue(config.Release.Disable) {
		t.Errorf("release.disable = %v, want release enabled", config.Release.Disable)
	}
	if config.Release.Draft == nil || *config.Release.Draft {
		t.Error("release.draft must be false")
	}
	if config.Release.Prerelease != "auto" {
		t.Errorf("release.prerelease = %q, want auto", config.Release.Prerelease)
	}
	if !config.Release.Replace {
		t.Error("release.replace_existing_artifacts must be true")
	}
	if isTrue(config.Changelog.Disable) {
		t.Errorf("changelog.disable = %v, want the release notes generated", config.Changelog.Disable)
	}
	if config.Checksum.NameTemplate != "checksums.txt" {
		t.Errorf("checksum.name_template = %q, want checksums.txt (subject-checksums of release.yml)", config.Checksum.NameTemplate)
	}
}

// isTrue reports whether a goreleaser boolean field, a bool or a template
// string, is set to true literally.
func isTrue(value any) bool {
	return value == true || value == "true"
}

// TestChangelogStart pins CHANGELOG_START of the Makefile, where the release
// notes begin while there is no final tag: the first commit of the rewrite,
// with the commits of the upstream project before it, two of which are in
// Conventional Commits form and would otherwise reach the notes.
func TestChangelogStart(t *testing.T) {
	data, err := os.ReadFile("Makefile")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^CHANGELOG_START := (\S+)$`).FindSubmatch(data)
	if m == nil {
		t.Fatal("Makefile has no CHANGELOG_START := line")
	}
	start := string(m[1])
	out, err := exec.Command("git", "rev-parse", "--is-shallow-repository").Output()
	if err != nil {
		t.Skipf("not a git work tree: %v", err)
	}
	if strings.TrimSpace(string(out)) == "true" {
		t.Skip("shallow clone: the upstream history is missing")
	}
	if out, err := exec.Command("git", "cat-file", "-e", start+"^{commit}").CombinedOutput(); err != nil {
		t.Fatalf("CHANGELOG_START %s is not a commit: %v %s", start, err, out)
	}
	for _, upstream := range []string{"f0e3359", "51af82c"} {
		if out, err := exec.Command("git", "merge-base", "--is-ancestor", upstream, start+"^").CombinedOutput(); err != nil {
			t.Errorf("upstream commit %s is not before CHANGELOG_START %s: %v %s", upstream, start, err, out)
		}
	}
}

// TestSnapshotTagOrder pins git.prerelease_suffix of .goreleaser.yaml: a
// snapshot takes the first tag of HEAD in git version order, and without the
// suffix git sorts v0.1.0-rc1 above v0.1.0 on a commit that carries both, so
// the snapshot would version 0.1.0-rc<time>, below the published 0.1.0.
func TestSnapshotTagOrder(t *testing.T) {
	data, err := os.ReadFile(".goreleaser.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Git struct {
			PrereleaseSuffix string `yaml:"prerelease_suffix"`
		} `yaml:"git"`
	}
	if err := yaml.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	if config.Git.PrereleaseSuffix != "-" {
		t.Fatalf("git.prerelease_suffix = %q, want -", config.Git.PrereleaseSuffix)
	}
	dir := t.TempDir()
	runIn(t, dir, "git", "init", "-q")
	runIn(t, dir, "git", "commit", "-q", "--allow-empty", "-m", "release")
	runIn(t, dir, "git", "tag", "v0.1.0-rc1")
	runIn(t, dir, "git", "tag", "v0.1.0")
	cmd := exec.Command("git", "-c", "versionsort.suffix="+config.Git.PrereleaseSuffix,
		"tag", "--points-at", "HEAD", "--sort", "-version:refname")
	cmd.Dir = dir
	cmd.Env = releaseEnv()
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if first, _, _ := strings.Cut(string(out), "\n"); first != "v0.1.0" {
		t.Errorf("first tag of HEAD = %q, want v0.1.0 above its rc:\n%s", first, out)
	}
}
