package main

import (
	"strings"
	"testing"
)

func TestCheckMessage(t *testing.T) {
	valid := []string{
		"feat(config): load targets from toml\n",
		"fix: keep the body when headers are malformed",
		"build(deps): bump the go-minor-patch group with 2 updates\n\nBumps x from 1 to 2.\n",
		"refactor!: rename the http target package",
		"ci(deps): bump actions/checkout from 7.0.0 to 7.0.1",
	}
	for _, msg := range valid {
		if problems := checkMessage("jane@example.org", msg); len(problems) != 0 {
			t.Errorf("checkMessage(%q) = %v, want none", msg, problems)
		}
	}
	invalid := map[string]string{
		"Bump github.com/slack-go/slack from 0.26.0 to 0.29.0": "not \"type(scope): description\"",
		"feature: add things":                       "not \"type(scope): description\"",
		"fix:missing space":                         "not \"type(scope): description\"",
		"fix(Config): uppercase scope":              "not \"type(scope): description\"",
		"fix: " + strings.Repeat("x", 68):           "limit 72",
		"docs: end with a period.":                  "ends with a period",
		"fix: subject\nbody glued to the subject\n": "no blank line",
		"WIP": "not \"type(scope): description\"",
	}
	for msg, want := range invalid {
		problems := checkMessage("jane@example.org", msg)
		if !strings.Contains(strings.Join(problems, "\n"), want) {
			t.Errorf("checkMessage(%q) = %v, want a problem containing %q", msg, problems, want)
		}
	}
}

// TestCheckMessageDependabot pins subjects that Dependabot produces with the
// commit-message settings of .github/dependabot.yml: they may exceed the
// length limit but must still follow the format.
func TestCheckMessageDependabot(t *testing.T) {
	subjects := []string{
		"build(deps): bump the go-minor-patch group across 2 directories with 3 updates",
		"build(deps): bump github.com/golangci/golangci-lint/v2 from 2.14.0 to 2.15.0 in /tools",
	}
	for _, subject := range subjects {
		if problems := checkMessage(dependabotEmail, subject+"\n\nBumps ...\n"); len(problems) != 0 {
			t.Errorf("dependabot subject %q: %v, want none", subject, problems)
		}
		if problems := checkMessage("jane@example.org", subject); len(problems) == 0 {
			t.Errorf("subject %q by a person passed, want the length limit", subject)
		}
	}
	if problems := checkMessage("jane@example.org", subjects[0]); len(problems) == 0 {
		t.Error("long subject passed for an author who only borrowed the Dependabot name")
	}
	if problems := checkMessage(dependabotEmail, "Bump x from 1 to 2"); len(problems) == 0 {
		t.Error("dependabot subject without type passed, want a format problem")
	}
}
