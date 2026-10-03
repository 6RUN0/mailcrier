package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// dockerfilePatterns match the Dockerfiles of the test images, each pinned
// by digest.
var dockerfilePatterns = []string{"testdata/*/Dockerfile", "packaging/smoke/*/Dockerfile"}

// TestDependabotCoversDockerfiles fails when the directory of a test image
// is missing from the docker entries of .github/dependabot.yml, whose
// directories are an explicit list: the digest of such an image would
// never be updated. It also fails on a listed directory without a
// Dockerfile.
func TestDependabotCoversDockerfiles(t *testing.T) {
	data, err := os.ReadFile(".github/dependabot.yml")
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Updates []struct {
			Ecosystem   string   `yaml:"package-ecosystem"`
			Directory   string   `yaml:"directory"`
			Directories []string `yaml:"directories"`
		} `yaml:"updates"`
	}
	if err := yaml.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	listed := map[string]bool{}
	for _, update := range config.Updates {
		if update.Ecosystem != "docker" {
			continue
		}
		for _, dir := range append(update.Directories, update.Directory) {
			if dir != "" {
				listed[strings.TrimPrefix(dir, "/")] = true
			}
		}
	}
	found := map[string]bool{}
	for _, pattern := range dockerfilePatterns {
		files, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatal(err)
		}
		for _, file := range files {
			found[filepath.Dir(file)] = true
		}
	}
	for dir := range found {
		if !listed[dir] {
			t.Errorf("%s/Dockerfile has no docker entry in .github/dependabot.yml", dir)
		}
	}
	for dir := range listed {
		if !found[dir] {
			t.Errorf(".github/dependabot.yml lists %s, which has no Dockerfile", dir)
		}
	}
}
