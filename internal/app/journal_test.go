package app

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/6RUN0/mailcrier/internal/delivery"
	"github.com/6RUN0/mailcrier/internal/golden"
	"github.com/6RUN0/mailcrier/internal/spool"
)

// spoolID matches an entry id, whose tail is random.
var spoolID = regexp.MustCompile(`\b\d{19}-[0-9a-f]{16}\b`)

// journalText returns text with what changes from run to run replaced:
// the time of each record, the ids of entries and the spool directory.
func journalText(text, dir string) string {
	var out strings.Builder
	for _, line := range strings.SplitAfter(text, "\n") {
		out.WriteString(logTime.ReplaceAllString(line, ""))
	}
	return spoolID.ReplaceAllString(strings.ReplaceAll(out.String(), dir, "<spool>"), "<id>")
}

// TestJournalGolden pins the whole log of a few calls and queue runs,
// every record with its level, fields and order, and what the caller
// reads on stderr, in testdata/journal/<name>.txtar: substrings in other
// tests would let a key or a level drift. Rewrite the files with
// go test ./internal/app -run TestJournalGolden -update and review the
// diff.
func TestJournalGolden(t *testing.T) {
	cases := []struct {
		name string
		run  func(c *spoolCase) (int, *invocation)
	}{
		{"delivered", func(c *spoolCase) (int, *invocation) {
			return c.sendInput("Message-ID: <m1@example.org>\nSubject: s\n\nb\n")
		}},
		{"temp-queued", func(c *spoolCase) (int, *invocation) {
			c.service.reply("a", delivery.Temp)
			return c.sendInput("Message-ID: <m1@example.org>\nSubject: s\n\nb\n")
		}},
		{"lost-without-spool", func(c *spoolCase) (int, *invocation) {
			c.dir = filepath.Join(c.dir, "missing")
			c.service.reply("a", delivery.Temp)
			c.service.reply("b", delivery.Temp)
			return c.sendInput("Subject: s\n\nb\n")
		}},
		{"configuration-rejected", func(c *spoolCase) (int, *invocation) {
			c.config = "[target.a]\ntype = \"http\"\n"
			return c.sendInput("Subject: s\n\nb\n")
		}},
		{"queue-run-retry-and-corrupt", func(c *spoolCase) (int, *invocation) {
			c.service.reply("a", delivery.Temp)
			c.send("kept", elevatedUser)
			c.service.reply("a", delivery.Temp)
			c.send("broken", elevatedUser)
			ids := c.ids(spool.QueueDir)
			corruptSidecar(c, ids[1])
			c.clock.advance(time.Hour)
			c.service.reply("a", delivery.Temp)
			return c.queueRun(rootCaller)
		}},
		{"check-config-without-syslog", func(c *spoolCase) (int, *invocation) {
			inv := &invocation{config: twoTargets + "[[route]]\nsubject = \"*\"\ntargets = [\"a\"]\n", args: []string{"--check-config"}, creds: plainUser,
				stdin: iotest.ErrReader(errors.New("stdin read")), isSyslogDown: true}
			return inv.run(c.t), inv
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newSpoolCase(t)
			dir := c.dir
			code, inv := tc.run(c)
			golden.Check(t, filepath.Join("testdata", "journal", tc.name+".txtar"), golden.Archive{Sections: []golden.Section{
				{Name: "exit", Data: []byte(strconv.Itoa(code))},
				{Name: "log", Data: []byte(journalText(callField.ReplaceAllString(inv.log("mailcrier"), ""), dir))},
				{Name: "stderr", Data: []byte(journalText(callField.ReplaceAllString(inv.stderr.String(), ""), dir))},
			}})
		})
	}
}

// corruptSidecar overwrites the sidecar of the queued entry id with text
// that does not decode.
func corruptSidecar(c *spoolCase, id string) {
	c.t.Helper()
	if err := os.WriteFile(filepath.Join(c.dir, spool.QueueDir, id+".json"), []byte("{"), 0o660); err != nil {
		c.t.Fatal(err)
	}
}
