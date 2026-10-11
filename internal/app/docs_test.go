package app

import (
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
	"testing/iotest"

	"github.com/6RUN0/mailcrier/internal/backend/hook"
	"github.com/6RUN0/mailcrier/internal/config"
)

// userDocs returns the paths of README.md and every page under docs/.
func userDocs(t *testing.T) []string {
	t.Helper()
	docs := []string{"../../README.md"}
	err := filepath.WalkDir("../../docs", func(path string, entry fs.DirEntry, err error) error {
		if err == nil && !entry.IsDir() && strings.HasSuffix(path, ".md") {
			docs = append(docs, path)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return docs
}

// TestDocTemplatesParse builds the targets of the documentation examples
// that carry a template, so that every example template parses.
func TestDocTemplatesParse(t *testing.T) {
	t.Parallel()
	files := fstest.MapFS{"etc/mailcrier.d/telegram.token": {Data: []byte("123:abc\n")}, "etc/mailcrier.d/mm.url": {Data: []byte("https://mm.example.org/hooks/x\n")}}
	found := 0
	for _, path := range userDocs(t) {
		doc, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, block := range regexp.MustCompile("(?s)```toml\n(.*?)```").FindAllStringSubmatch(string(doc), -1) {
			if !strings.Contains(block[1], "template") {
				continue
			}
			found++
			files["etc/mailcrier.conf"] = &fstest.MapFile{Data: []byte(block[1])}
			cfg, err := config.Load(files, "etc/mailcrier.conf")
			if err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			if _, err := buildTargets(cfg, &http.Client{}, hook.Process{}); err != nil {
				t.Errorf("%s: block with a template: %v", path, err)
			}
		}
	}
	if found == 0 {
		t.Error("the documentation has no toml block with a template")
	}
}

// checkExample is the example of docs/checking.md: the first toml block
// and, after it, the text block of the output of --check-config.
var checkExample = regexp.MustCompile("(?s)```toml\n(.*?)```.*?```text\n(.*?)```")

// TestDocCheckConfigOutput runs --check-config on the example of
// docs/checking.md and compares its stderr with the output shown there.
func TestDocCheckConfigOutput(t *testing.T) {
	t.Parallel()
	doc, err := os.ReadFile("../../docs/checking.md")
	if err != nil {
		t.Fatal(err)
	}
	example := checkExample.FindStringSubmatch(string(doc))
	if example == nil {
		t.Fatal("docs/checking.md has no example of --check-config")
	}
	inv := &invocation{config: example[1], args: []string{"--check-config"}, creds: plainUser, stdin: iotest.ErrReader(errors.New("stdin read"))}
	if code := inv.run(t); code != 78 {
		t.Fatalf("Run() = %d, want 78 for the error of the example; output:\n%s", code, inv.output())
	}
	if got := inv.stderr.String(); got != example[2] {
		t.Errorf("stderr\n%s\ndocs/checking.md\n%s", got, example[2])
	}
}
