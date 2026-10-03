package main

import (
	"os"
	"slices"
	"testing"

	"go.yaml.in/yaml/v3"
)

// TestCIBranches fails when the push branches of ci.yml or codeql.yml
// differ from develop and main, or their pull_request branches differ from
// develop: main only fast-forwards, so a green check on a pull request into
// it would invite a merge commit there.
func TestCIBranches(t *testing.T) {
	for _, file := range []string{".github/workflows/ci.yml", ".github/workflows/codeql.yml"} {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var workflow struct {
			On struct {
				Push struct {
					Branches []string `yaml:"branches"`
				} `yaml:"push"`
				PullRequest struct {
					Branches []string `yaml:"branches"`
				} `yaml:"pull_request"`
			} `yaml:"on"`
		}
		if err := yaml.Unmarshal(data, &workflow); err != nil {
			t.Fatal(err)
		}
		push := slices.Sorted(slices.Values(workflow.On.Push.Branches))
		if want := []string{"develop", "main"}; !slices.Equal(push, want) {
			t.Errorf("%s: push branches %q, want %q", file, push, want)
		}
		if pr, want := workflow.On.PullRequest.Branches, []string{"develop"}; !slices.Equal(pr, want) {
			t.Errorf("%s: pull_request branches %q, want %q", file, pr, want)
		}
	}
}
