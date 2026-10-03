package app

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/6RUN0/mailcrier/internal/golden"
)

// callersDir holds one message and one command line per caller of
// sendmail: <name>.eml is the input, <name>.argv the command line with
// argv[0] first, one argument per line.
const callersDir = "../../testdata/callers"

// logTime is the time field of a log record, which golden files leave out.
var logTime = regexp.MustCompile(`^time=\S+ `)

// TestCallers runs every caller fixture through Run with two recording
// targets and compares exit status, envelope, subject, body and warnings with
// testdata/callers/<name>.txtar.
func TestCallers(t *testing.T) {
	argvFiles, err := filepath.Glob(filepath.Join(callersDir, "*.argv"))
	if err != nil {
		t.Fatal(err)
	}
	if len(argvFiles) == 0 {
		t.Fatalf("no fixtures in %s", callersDir)
	}
	for _, argvFile := range argvFiles {
		name := strings.TrimSuffix(filepath.Base(argvFile), ".argv")
		t.Run("T-CALL-32/"+name, func(t *testing.T) {
			argvData, err := os.ReadFile(argvFile)
			if err != nil {
				t.Fatal(err)
			}
			input, err := os.ReadFile(filepath.Join(callersDir, name+".eml"))
			if err != nil {
				t.Fatal(err)
			}
			argv := strings.Split(strings.TrimSuffix(string(argvData), "\n"), "\n")
			rec := &recorder{}
			inv := &invocation{
				program: argv[0],
				args:    append([]string{}, argv[1:]...),
				config:  twoTargets,
				stdin:   strings.NewReader(string(input)),
				deliver: rec.deliver,
			}
			code := inv.run(t)
			golden.Check(t, filepath.Join("testdata", "callers", name+".txtar"), callerArchive(code, rec, inv))
		})
	}
}

// callerArchive renders the outcome of one fixture.
func callerArchive(code int, rec *recorder, inv *invocation) golden.Archive {
	envelope := fmt.Sprintf("sender: %q\nsender-name: %q\nrecipients: %q\n", rec.env.Sender, rec.env.SenderName, rec.env.Recipients)
	var warnings strings.Builder
	for _, line := range strings.Split(inv.log("mailcrier"), "\n") {
		if strings.Contains(line, "level=WARN") {
			warnings.WriteString(logTime.ReplaceAllString(line, "") + "\n")
		}
	}
	sections := []golden.Section{
		{Name: "exit", Data: []byte(strconv.Itoa(code))},
		{Name: "envelope", Data: []byte(envelope)},
		{Name: "payloads", Data: []byte(strconv.Itoa(len(rec.data)))},
	}
	if len(rec.data) > 0 {
		sections = append(sections,
			golden.Section{Name: "title", Data: []byte(rec.data[0].Subject)},
			golden.Section{Name: "text", Data: []byte(rec.data[0].Body)},
		)
	}
	sections = append(sections, golden.Section{Name: "warnings", Data: []byte(warnings.String())})
	return golden.Archive{Sections: sections}
}
