package main

import (
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// scheduleOnlyJobs are the jobs of ci.yml that do not run on push and so
// cannot gate a release. A new job that skips push needs a decision here.
var scheduleOnlyJobs = []string{"make vuln"}

// stringList takes a YAML scalar or sequence of strings, as needs does.
type stringList []string

func (l *stringList) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		*l = []string{node.Value}
		return nil
	}
	var list []string
	if err := node.Decode(&list); err != nil {
		return err
	}
	*l = list
	return nil
}

type workflowFile struct {
	On          map[string]any     `yaml:"on"`
	Permissions *map[string]string `yaml:"permissions"`
	Jobs        map[string]struct {
		Name        string            `yaml:"name"`
		If          string            `yaml:"if"`
		Needs       stringList        `yaml:"needs"`
		Permissions map[string]string `yaml:"permissions"`
		Strategy    struct {
			Matrix map[string][]string `yaml:"matrix"`
		} `yaml:"strategy"`
		Steps []struct {
			Uses string         `yaml:"uses"`
			With map[string]any `yaml:"with"`
			Run  string         `yaml:"run"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

func readWorkflow(t *testing.T, file string) workflowFile {
	t.Helper()
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var workflow workflowFile
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatalf("%s: %v", file, err)
	}
	return workflow
}

// TestReleaseWorkflow pins what makes release.yml safe to run on a tag: it
// starts on a v tag only, grants nothing by default and write access only
// to the job that publishes after the gate and the vulnerability check,
// restores no cache another workflow could have poisoned, leaves no token
// in the checkout, expands no expression inside a shell script, and keeps
// every step reproducible through make. It also keeps RELEASE_JOBS of the
// Makefile equal to the jobs ci.yml runs on push, so a renamed or added
// job cannot leave the gate waiting for a job that never runs or skipping
// one that does.
func TestReleaseWorkflow(t *testing.T) {
	const file = ".github/workflows/release.yml"
	workflow := readWorkflow(t, file)

	if len(workflow.On) != 1 || workflow.On["push"] == nil {
		t.Errorf("%s: triggers %v, want push only", file, workflow.On)
	}
	push, _ := workflow.On["push"].(map[string]any)
	if fmt.Sprint(push) != "map[tags:[v*]]" {
		t.Errorf("%s: push %v, want tags [v*] only", file, push)
	}
	if workflow.Permissions == nil || len(*workflow.Permissions) != 0 {
		t.Errorf("%s: top-level permissions must be {}", file)
	}
	for id, job := range workflow.Jobs {
		for scope, access := range job.Permissions {
			if access == "write" && id != "publish" {
				t.Errorf("%s: job %s has %s: write; only publish may write", file, id, scope)
			}
		}
		for _, step := range job.Steps {
			action, _, _ := strings.Cut(step.Uses, "@")
			if action == "actions/setup-go" && step.With["cache"] != false {
				t.Errorf("%s: job %s: setup-go without cache: false", file, id)
			}
			if action == "astral-sh/setup-uv" && step.With["enable-cache"] != false {
				t.Errorf("%s: job %s: setup-uv without enable-cache: false", file, id)
			}
			if step.Run != "" && !strings.HasPrefix(step.Run, "make ") {
				t.Errorf("%s: job %s: run %q does not start with make", file, id, step.Run)
			}
			if strings.Contains(step.Run, "${{") {
				t.Errorf("%s: job %s: run %q expands an expression; pass it through env", file, id, step.Run)
			}
			if action == "actions/checkout" && step.With["persist-credentials"] != false {
				t.Errorf("%s: job %s: checkout without persist-credentials: false", file, id)
			}
		}
	}
	publish, ok := workflow.Jobs["publish"]
	if !ok {
		t.Fatalf("%s: no job publish", file)
	}
	for _, need := range []string{"gate", "vuln"} {
		if !slices.Contains(publish.Needs, need) {
			t.Errorf("%s: publish needs %q, want %s among them", file, publish.Needs, need)
		}
	}

	ci := readWorkflow(t, ".github/workflows/ci.yml")
	matrixRef := regexp.MustCompile(`\$\{\{ *matrix\.([A-Za-z0-9_-]+) *\}\}`)
	var onPush, notOnPush []string
	for id, job := range ci.Jobs {
		names := []string{job.Name}
		if m := matrixRef.FindStringSubmatch(job.Name); m != nil {
			names = nil
			for _, value := range job.Strategy.Matrix[m[1]] {
				names = append(names, strings.Replace(job.Name, m[0], value, 1))
			}
		}
		switch job.If {
		case "", "github.event_name != 'schedule'":
			onPush = append(onPush, names...)
		case "github.event_name == 'schedule'":
			notOnPush = append(notOnPush, names...)
		default:
			t.Errorf("ci.yml: job %s: condition %q unknown to this test; decide whether it runs on push", id, job.If)
		}
	}
	slices.Sort(onPush)
	slices.Sort(notOnPush)
	if want := slices.Sorted(slices.Values(scheduleOnlyJobs)); !slices.Equal(notOnPush, want) {
		t.Errorf("ci.yml: jobs not run on push %q, want %q", notOnPush, want)
	}
	if gate := releaseJobs(t); !slices.Equal(gate, onPush) {
		t.Errorf("Makefile RELEASE_JOBS %q, want the jobs ci.yml runs on push %q", gate, onPush)
	}
}

// releaseJobs returns the sorted job names of RELEASE_JOBS in the Makefile.
func releaseJobs(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile("Makefile")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^RELEASE_JOBS := (.+)$`).FindSubmatch(data)
	if m == nil {
		t.Fatal("Makefile has no RELEASE_JOBS := line")
	}
	return slices.Sorted(slices.Values(strings.Split(string(m[1]), ",")))
}
