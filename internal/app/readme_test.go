package app

import (
	"errors"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
	"testing/iotest"

	"github.com/6RUN0/mailcrier/internal/backend/hook"
	"github.com/6RUN0/mailcrier/internal/config"
)

// TestReadmeTemplatesParse builds the targets of the README examples that
// carry a template, so that every example template parses.
func TestReadmeTemplatesParse(t *testing.T) {
	readme, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	files := fstest.MapFS{"etc/slendmail.d/tg.token": {Data: []byte("123:abc\n")}, "etc/slendmail.d/mm.url": {Data: []byte("https://mm.example.org/hooks/x\n")}}
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

// readmeCheckExample is the example of the README section "Checking the
// configuration": a toml block and, after it, the text block of the
// output of --check-config.
var readmeCheckExample = regexp.MustCompile("(?s)### Checking the configuration\n.*?```toml\n(.*?)```.*?```text\n(.*?)```")

// TestReadmeCheckConfigOutput runs --check-config on the example of the
// README and compares its stderr with the output shown there.
func TestReadmeCheckConfigOutput(t *testing.T) {
	readme, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	example := readmeCheckExample.FindStringSubmatch(string(readme))
	if example == nil {
		t.Fatal("README has no example of --check-config")
	}
	inv := &invocation{config: example[1], args: []string{"--check-config"}, creds: plainUser, stdin: iotest.ErrReader(errors.New("stdin read"))}
	if code := inv.run(t); code != 0 {
		t.Fatalf("Run() = %d, want 0; output:\n%s", code, inv.output())
	}
	if got := inv.stderr.String(); got != example[2] {
		t.Errorf("stderr\n%s\nREADME\n%s", got, example[2])
	}
}
