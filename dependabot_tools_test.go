package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var (
	toolBlock      = regexp.MustCompile(`(?s)\ntool \((.*?)\n\)`)
	toolLine       = regexp.MustCompile(`(?m)^tool ([^\s(]\S*)$`)
	requireLine    = regexp.MustCompile(`(?m)^\s+(\S+) v\S+`)
	allowedToolDep = regexp.MustCompile(`dependency-name: "([^"]+)"\s+dependency-type: all`)
)

// TestDependabotCoversTools fails when a module providing a tool in a
// tools/**/go.mod is missing from the allow list of .github/dependabot.yml.
// go get -tool records such modules as indirect requirements, and
// Dependabot skips indirect Go modules unless an allow entry names them with
// dependency-type all, so a tool without an entry silently stops updating.
func TestDependabotCoversTools(t *testing.T) {
	config, err := os.ReadFile(".github/dependabot.yml")
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{}
	for _, match := range allowedToolDep.FindAllStringSubmatch(string(config), -1) {
		allowed[match[1]] = true
	}
	modFiles, err := filepath.Glob("tools/*/go.mod")
	if err != nil {
		t.Fatal(err)
	}
	modFiles = append(modFiles, "tools/go.mod")
	for _, modFile := range modFiles {
		data, err := os.ReadFile(modFile)
		if err != nil {
			t.Fatal(err)
		}
		tools := toolPackages(string(data))
		if len(tools) == 0 {
			t.Errorf("%s declares no tool", modFile)
		}
		modules := requireLine.FindAllStringSubmatch(string(data), -1)
		for _, tool := range tools {
			module := providingModule(tool, modules)
			if module == "" {
				t.Errorf("%s: no requirement provides tool %s", modFile, tool)
				continue
			}
			if !allowed[module] {
				t.Errorf("%s: tool module %s has no allow entry with dependency-type all in .github/dependabot.yml", modFile, module)
			}
		}
	}
}

func toolPackages(goMod string) []string {
	var tools []string
	if block := toolBlock.FindStringSubmatch(goMod); block != nil {
		tools = append(tools, strings.Fields(block[1])...)
	}
	for _, match := range toolLine.FindAllStringSubmatch(goMod, -1) {
		tools = append(tools, match[1])
	}
	return tools
}

// providingModule returns the longest required module path that is a
// prefix of the tool package path.
func providingModule(tool string, requirements [][]string) string {
	best := ""
	for _, requirement := range requirements {
		module := requirement[1]
		if (tool == module || strings.HasPrefix(tool, module+"/")) && len(module) > len(best) {
			best = module
		}
	}
	return best
}
