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

// TestMutantsConcurrency fails when the jobs of mutants.yml share a
// concurrency group: a manual run of the 30-minute contract job would wait
// for the mutation run of up to 5 hours.
func TestMutantsConcurrency(t *testing.T) {
	data, err := os.ReadFile(".github/workflows/mutants.yml")
	if err != nil {
		t.Fatal(err)
	}
	type concurrency struct {
		Group string `yaml:"group"`
	}
	var workflow struct {
		Concurrency *concurrency `yaml:"concurrency"`
		Jobs        map[string]struct {
			Concurrency *concurrency `yaml:"concurrency"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	if workflow.Concurrency != nil {
		t.Errorf("workflow-level concurrency group %q holds every job", workflow.Concurrency.Group)
	}
	groups := map[string]string{}
	for name, job := range workflow.Jobs {
		if job.Concurrency == nil || job.Concurrency.Group == "" {
			t.Errorf("job %s has no concurrency group", name)
			continue
		}
		if other, ok := groups[job.Concurrency.Group]; ok {
			t.Errorf("jobs %s and %s share the concurrency group %q", other, name, job.Concurrency.Group)
		}
		groups[job.Concurrency.Group] = name
	}
}
