package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var licenseExceptionsLine = regexp.MustCompile(`(?m)^LICENSE_EXCEPTIONS := (.*)$`)

// offeredLicenses checks, per module of LICENSE_EXCEPTIONS in the
// Makefile, that the module in the version go.mod selects still offers
// the license it is taken under. A module without an entry fails the
// test, so an exception cannot be added without its check.
var offeredLicenses = map[string]func(t *testing.T, dir string){
	// Taken under EDL-1.0, which is BSD-3-Clause, out of EPL-2.0 or
	// EDL-1.0.
	"github.com/eclipse/paho.golang": func(t *testing.T, dir string) {
		license := readModuleFile(t, dir, "LICENSE")
		if !strings.Contains(license, "under the terms of the Eclipse Public License v2.0 and Eclipse Distribution License v1.0") {
			t.Error("LICENSE no longer offers the Eclipse Distribution License v1.0")
		}
		edl := readModuleFile(t, dir, "edl-v10")
		for _, clause := range []string{
			"Eclipse Distribution License - v 1.0",
			"Redistribution and use in source and binary forms, with or without modification, are permitted provided that the following conditions are met:",
			"Neither the name of the Eclipse Foundation, Inc. nor the names of its contributors may be used to endorse or promote products derived from this software without specific prior written permission.",
		} {
			if !strings.Contains(edl, clause) {
				t.Errorf("edl-v10 lacks %q", clause)
			}
		}
	},
}

// TestLicenseExceptionsStillOffered fails when a module that make
// licenses lets through under a license go-licenses does not detect no
// longer offers that license, as after an update that relicenses it.
func TestLicenseExceptionsStillOffered(t *testing.T) {
	makefile, err := os.ReadFile("Makefile")
	if err != nil {
		t.Fatal(err)
	}
	match := licenseExceptionsLine.FindSubmatch(makefile)
	if match == nil {
		t.Fatal("Makefile has no LICENSE_EXCEPTIONS line")
	}
	modules := strings.Fields(string(match[1]))
	if len(modules) == 0 {
		t.Fatal("LICENSE_EXCEPTIONS is empty; drop the line and this test instead")
	}
	for _, module := range modules {
		t.Run(module, func(t *testing.T) {
			check, ok := offeredLicenses[module]
			if !ok {
				t.Fatalf("no check of the offered license for %s in offeredLicenses", module)
			}
			check(t, moduleDir(t, module))
		})
	}
}

// moduleDir returns the directory of module in the module cache, in the
// version go.mod selects, downloading it when needed.
func moduleDir(t *testing.T, module string) string {
	t.Helper()
	out, err := exec.Command("go", "mod", "download", "-json", module).Output()
	if err != nil {
		t.Fatalf("go mod download %s: %v", module, err)
	}
	var info struct{ Dir, Error string }
	if err := json.Unmarshal(out, &info); err != nil || info.Dir == "" {
		t.Fatalf("go mod download %s: %v %s", module, err, info.Error)
	}
	return info.Dir
}

// readModuleFile returns a file of a module with its white space
// collapsed, so that a rewrapped text still matches.
func readModuleFile(t *testing.T, dir, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(strings.Fields(string(data)), " ")
}
