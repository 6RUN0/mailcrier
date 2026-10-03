package main

import (
	"os"
	"testing"

	"go.yaml.in/yaml/v3"
)

// TestDependabotTargetsDevelop fails when an entry of .github/dependabot.yml
// lacks target-branch develop: Dependabot then opens its version updates
// against the default branch main, which only fast-forwards to a commit of
// develop: before the first release also to one that changes the CI
// infrastructure, after it only to a released one.
func TestDependabotTargetsDevelop(t *testing.T) {
	data, err := os.ReadFile(".github/dependabot.yml")
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Updates []struct {
			Ecosystem    string `yaml:"package-ecosystem"`
			TargetBranch string `yaml:"target-branch"`
		} `yaml:"updates"`
	}
	if err := yaml.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	if len(config.Updates) == 0 {
		t.Fatal(".github/dependabot.yml has no updates entry")
	}
	for i, update := range config.Updates {
		if update.TargetBranch != "develop" {
			t.Errorf("updates[%d] (%s): target-branch is %q, want \"develop\"", i, update.Ecosystem, update.TargetBranch)
		}
	}
}
