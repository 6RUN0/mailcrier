package render

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/6RUN0/slendmail/internal/golden"
	"github.com/6RUN0/slendmail/internal/message"
	"github.com/6RUN0/slendmail/internal/sendmail"
	"github.com/6RUN0/slendmail/internal/text"
)

// callersDir holds the caller fixtures: <name>.eml and <name>.argv.
const callersDir = "../../testdata/callers"

// builtinFormats are the formats with a built-in template, in golden file
// order.
var builtinFormats = []text.Format{
	text.FormatPlain, text.FormatTelegramHTML, text.FormatTelegramMarkdownV2, text.FormatSlackMrkdwn,
	text.FormatDiscord, text.FormatNtfy, text.FormatMattermost, text.FormatSlackWebhook, text.FormatGenericJSON,
}

// jsonFormats render a JSON request body.
var jsonFormats = map[text.Format]bool{text.FormatMattermost: true, text.FormatSlackWebhook: true, text.FormatGenericJSON: true}

// testReceivedAt is the receive time of the fixtures.
var testReceivedAt = time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)

// fixtureData reads one caller fixture as a real invocation would and
// returns its template data.
func fixtureData(t *testing.T, name string) Data {
	t.Helper()
	argv, err := os.ReadFile(filepath.Join(callersDir, name+".argv"))
	if err != nil {
		t.Fatal(err)
	}
	input, err := os.ReadFile(filepath.Join(callersDir, name+".eml"))
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSuffix(string(argv), "\n"), "\n")
	inv, _, err := sendmail.Parse(args[0], args[1:])
	if err != nil {
		t.Fatal(err)
	}
	msg, blind, _, err := message.Read(strings.NewReader(string(input)), message.ReadOptions{IgnoreDots: inv.IgnoreDots, MaxSize: message.MaxSize, ReceivedAt: testReceivedAt})
	if err != nil {
		t.Fatal(err)
	}
	env := inv.Envelope(msg, blind, func() string { return "alice@host1.example.org" })
	d := NewData(msg, env, blind)
	d.Hostname = "host1.example.org"
	d.ReceivedAt = testReceivedAt
	return d
}

// TestBuiltinGolden renders every caller fixture with every built-in
// template and compares the output with testdata/golden/<name>.txtar.
func TestBuiltinGolden(t *testing.T) {
	argvFiles, err := filepath.Glob(filepath.Join(callersDir, "*.argv"))
	if err != nil {
		t.Fatal(err)
	}
	if len(argvFiles) == 0 {
		t.Fatalf("no fixtures in %s", callersDir)
	}
	templates := map[text.Format]*Template{}
	for _, format := range builtinFormats {
		tmpl, err := Builtin(format)
		if err != nil {
			t.Fatal(err)
		}
		templates[format] = tmpl
	}
	for _, argvFile := range argvFiles {
		name := strings.TrimSuffix(filepath.Base(argvFile), ".argv")
		t.Run("T-TPL-01/"+name, func(t *testing.T) {
			d := fixtureData(t, name)
			var archive golden.Archive
			for _, format := range builtinFormats {
				out, err := templates[format].Execute(d)
				if err != nil {
					t.Fatalf("%s: %v", format, err)
				}
				if jsonFormats[format] && !json.Valid([]byte(out)) {
					t.Errorf("%s: invalid JSON: %s", format, out)
				}
				archive.Sections = append(archive.Sections, golden.Section{Name: string(format), Data: []byte(out)})
			}
			golden.Check(t, filepath.Join("testdata", "golden", name+".txtar"), archive)
		})
	}
}
