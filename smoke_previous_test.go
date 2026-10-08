package main

import (
	"cmp"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// unpublishedTags name releases whose run published nothing, so the upgrade
// tests cannot fetch them.
var unpublishedTags = []string{"v0.1.0-rc.1"}

// releaseTagVersion matches a release tag. v0.1.0-rc.2 is a published tag
// with a dot before the rc number, which RELEASE_TAG refuses.
var releaseTagVersion = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)(?:-rc\.?(\d+))?$`)

// tagOrder returns the parts of a release tag in the order they compare:
// major, minor, patch, 1 for a final release and 0 for an rc, rc number.
func tagOrder(tag string) ([]int, bool) {
	m := releaseTagVersion.FindStringSubmatch(tag)
	if m == nil {
		return nil, false
	}
	order := make([]int, 5)
	for i, part := range []string{m[1], m[2], m[3]} {
		order[i], _ = strconv.Atoi(part)
	}
	if m[4] == "" {
		order[3] = 1
	} else {
		order[4], _ = strconv.Atoi(m[4])
	}
	return order, true
}

// checkPreviousPin tells whether pin is the release the upgrade tests must
// start from, given the tags merged into HEAD and not on it: the newest final release, or
// before the first one the newest published rc.
func checkPreviousPin(pin string, merged []string) error {
	if !slices.Contains(merged, pin) {
		return fmt.Errorf("SMOKE_PREVIOUS %s is not a tag in the history of HEAD", pin)
	}
	var newest string
	var newestOrder []int
	for _, tag := range merged {
		order, ok := tagOrder(tag)
		if !ok || slices.Contains(unpublishedTags, tag) {
			continue
		}
		if newest == "" || order[3] > newestOrder[3] || order[3] == newestOrder[3] && slices.Compare(order, newestOrder) > 0 {
			newest, newestOrder = tag, order
		}
	}
	if pin != newest {
		return fmt.Errorf("SMOKE_PREVIOUS is %s, the newest release is %s: bump it per docs/releasing.md", pin, cmp.Or(newest, "none"))
	}
	return nil
}

// makeVariable returns the value of a "NAME := value" line of the Makefile.
func makeVariable(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile("Makefile")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^` + name + ` := (.+)$`).FindSubmatch(data)
	if m == nil {
		t.Fatalf("Makefile has no %s := line", name)
	}
	return string(m[1])
}

// TestSmokePrevious fails when SMOKE_PREVIOUS of the Makefile lags the
// newest release in the history of HEAD, so the upgrade smoke tests never
// start from a stale release, or when its checksum pin is malformed. A tag
// on HEAD itself does not count: the tagged commit cannot carry the pin to
// its own release, and a rerun of its CI must stay green for the release
// gate. Outside its own git work tree, in a shallow clone and in one
// without tags it is skipped, unless CI is set.
func TestSmokePrevious(t *testing.T) {
	if sums := makeVariable(t, "SMOKE_PREVIOUS_SUMS"); !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(sums) {
		t.Errorf("SMOKE_PREVIOUS_SUMS %q is not a sha256 in hex", sums)
	}
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := trackedFiles(dir); errors.Is(err, errNotOwnTree) {
		if os.Getenv("CI") != "" {
			t.Fatalf("%v, CI must check SMOKE_PREVIOUS against the tags", err)
		}
		t.Skipf("%v, no tags to check SMOKE_PREVIOUS against", err)
	}
	shallow, err := exec.Command("git", "rev-parse", "--is-shallow-repository").Output()
	if err != nil {
		t.Fatalf("git rev-parse --is-shallow-repository: %v", err)
	}
	tags, err := exec.Command("git", "tag", "--list", "v*").Output()
	if err != nil {
		t.Fatalf("git tag --list: %v", err)
	}
	if strings.TrimSpace(string(shallow)) == "true" || len(tags) == 0 {
		const reason = "a shallow clone or one without its tags (git clone --no-tags)"
		if os.Getenv("CI") != "" {
			t.Fatalf("%s: CI must check SMOKE_PREVIOUS against the tags, checkout needs fetch-depth: 0", reason)
		}
		t.Skipf("%s: fetch the history and tags to check SMOKE_PREVIOUS", reason)
	}
	out, err := exec.Command("git", "tag", "--merged", "HEAD", "--no-contains", "HEAD", "--list", "v*").Output()
	if err != nil {
		t.Fatalf("git tag --merged HEAD --no-contains HEAD: %v", err)
	}
	if err := checkPreviousPin(makeVariable(t, "SMOKE_PREVIOUS"), strings.Fields(string(out))); err != nil {
		t.Error(err)
	}
}

// TestCheckPreviousPin pins the choice of the release the upgrade tests
// start from: a final release wins over any rc, an rc counts only before
// the first final one, an unpublished rc never, and a tag outside the
// history of HEAD is refused.
func TestCheckPreviousPin(t *testing.T) {
	for _, c := range []struct {
		pin    string
		merged []string
		isOK   bool
	}{
		{"v0.1.0-rc.2", []string{"v0.1.0-rc.1", "v0.1.0-rc.2"}, true},
		{"v0.1.0-rc.2", []string{"v0.1.0-rc.1", "v0.1.0-rc.2", "v0.1.0-rc3"}, false},
		{"v0.1.0-rc3", []string{"v0.1.0-rc.2", "v0.1.0-rc3", "v0.1.0-rc10"}, false},
		{"v0.1.0-rc10", []string{"v0.1.0-rc.2", "v0.1.0-rc3", "v0.1.0-rc10"}, true},
		{"v0.1.0-rc3", []string{"v0.1.0-rc3", "v0.1.0"}, false},
		{"v0.1.0", []string{"v0.1.0-rc3", "v0.1.0", "v0.2.0-rc1"}, true},
		{"v0.10.0", []string{"v0.9.0", "v0.10.0"}, true},
		{"v0.1.0-rc.1", []string{"v0.1.0-rc.1"}, false},
		{"v0.1.0-rc3", []string{"v0.1.0-rc.2"}, false},
	} {
		if err := checkPreviousPin(c.pin, c.merged); (err == nil) != c.isOK {
			t.Errorf("checkPreviousPin(%s, %v) = %v, want ok %v", c.pin, c.merged, err, c.isOK)
		}
	}
}
