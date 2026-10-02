package app

import (
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/6RUN0/slendmail/internal/backend/hook"
	"github.com/6RUN0/slendmail/internal/config"
)

// TestReadmeTemplatesParse builds the targets of the README examples that
// carry a template, so that every example template parses.
func TestReadmeTemplatesParse(t *testing.T) {
	readme, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	files := fstest.MapFS{"etc/slendmail.d/tg.token": {Data: []byte("123:abc\n")}}
	found := 0
	for _, block := range regexp.MustCompile("(?s)```toml\n(.*?)```").FindAllStringSubmatch(string(readme), -1) {
		if !strings.Contains(block[1], "template") {
			continue
		}
		found++
		files["etc/slendmail.conf"] = &fstest.MapFile{Data: []byte(block[1])}
		cfg, err := config.Load(files, "etc/slendmail.conf")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := buildTargets(cfg, &http.Client{}, hook.Process{}); err != nil {
			t.Errorf("README block with a template: %v", err)
		}
	}
	if found == 0 {
		t.Error("README has no toml block with a template")
	}
}
