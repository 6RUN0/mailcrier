package app

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/6RUN0/slendmail/internal/config"
	"github.com/6RUN0/slendmail/internal/delivery"
	"github.com/6RUN0/slendmail/internal/route"
	"github.com/6RUN0/slendmail/internal/spool"
)

// routeRootToA routes the mail of root to target a and nothing else.
const routeRootToA = twoTargets + "\n[[route]]\nrecipient = \"root\"\ntargets = [\"a\"]\n"

// catchAllToB adds a route of every message to target b.
const catchAllToB = "\n[[route]]\ntargets = [\"b\"]\n"

// sendInput delivers input with args, cron's -ti when nil, as an elevated
// user, a millisecond after the previous message.
func (c *spoolCase) sendInput(input string, args ...string) (int, *invocation) {
	c.t.Helper()
	inv := c.invocation(elevatedUser, args)
	inv.stdin = strings.NewReader(input)
	code := inv.run(c.t)
	c.clock.advance(time.Millisecond)
	return code, inv
}

// isSpoolEmpty reports whether no area of the spool holds an entry.
func (c *spoolCase) isSpoolEmpty() bool {
	c.t.Helper()
	for _, area := range []string{spool.TmpDir, spool.QueueDir, spool.HoldDir, spool.FailedDir} {
		if len(c.ids(area)) != 0 {
			return false
		}
	}
	return true
}

// TestSuppress pins the [[suppress]] rules: a matching message goes
// nowhere, is not spooled, is logged with the number of the rule and
// exits 0.
func TestSuppress(t *testing.T) {
	t.Run("T-ADJ-33/subject-glob-ignores-case", suppressCase{
		rules: "[[suppress]]\nsubject = \"*ANACRON*\"\n", input: "Subject: Anacron job 'cron.daily' on host1\n\nb\n", rule: 1,
	}.check)
	t.Run("T-ADJ-33/cyrillic-encoded-subject", suppressCase{
		rules: "[[suppress]]\nsubject = \"*ошибка*\"\n", input: "Subject: =?utf-8?b?0J7QqNCY0JHQmtCQINC00LjRgdC60LA=?=\n\nb\n", rule: 1,
	}.check)
	t.Run("T-ADJ-33/sender-from-header", suppressCase{
		rules: "[[suppress]]\nsender = \"*@backup.example.org\"\n", input: "From: cron@BACKUP.example.org\nSubject: x\n\nb\n", rule: 1,
	}.check)
	t.Run("T-ADJ-33/second-rule-numbered", suppressCase{
		rules: "[[suppress]]\nsubject_regex = '^never$'\n\n[[suppress]]\nsubject_regex = 'disk (full|failed)'\n", input: "Subject: /dev/sda: disk failed\n\nb\n", rule: 2,
	}.check)
	t.Run("T-ADJ-33/encoded-word-fixture", func(t *testing.T) {
		argv, err := os.ReadFile(filepath.Join(callersDir, "apt-listchanges-rfc2047.argv"))
		if err != nil {
			t.Fatal(err)
		}
		input, err := os.ReadFile(filepath.Join(callersDir, "apt-listchanges-rfc2047.eml"))
		if err != nil {
			t.Fatal(err)
		}
		args := strings.Split(strings.TrimSuffix(string(argv), "\n"), "\n")[1:]
		suppressCase{rules: "[[suppress]]\nsubject = \"*NEWS FOR HÔST\"\n", input: string(input), args: args, rule: 1}.check(t)
	})
	t.Run("all-suppressed", suppressCase{
		rules: "[[suppress]]\nsubject = \"*\"\n", input: "To: root\nSubject: x\n\nb\n", rule: 1,
	}.check)
	t.Run("not-matching-delivered", func(t *testing.T) {
		c := newSpoolCase(t)
		c.config = twoTargets + "\n[[suppress]]\nsubject = \"*anacron*\"\n"
		code, inv := c.sendInput("Subject: disk\n\nb\n")
		if code != 0 || len(c.service.got("a")) != 1 || len(c.service.got("b")) != 1 || strings.Contains(inv.output(), "suppressed") {
			t.Errorf("Run() = %d, a %v, b %v; output:\n%s", code, c.service.got("a"), c.service.got("b"), inv.output())
		}
	})
}

type suppressCase struct {
	rules, input string
	args         []string
	rule         int
}

func (tc suppressCase) check(t *testing.T) {
	t.Helper()
	c := newSpoolCase(t)
	c.config = twoTargets + "\n" + tc.rules
	code, inv := c.sendInput(tc.input, tc.args...)
	if code != 0 || len(c.service.got("a"))+len(c.service.got("b")) != 0 {
		t.Fatalf("Run() = %d, a %v, b %v, want 0 and nothing sent; output:\n%s", code, c.service.got("a"), c.service.got("b"), inv.output())
	}
	if want := `level=INFO msg="message suppressed" rule=` + string(rune('0'+tc.rule)); !strings.Contains(inv.output(), want) {
		t.Errorf("output lacks %s:\n%s", want, inv.output())
	}
	if !c.isSpoolEmpty() {
		t.Error("suppressed message spooled")
	}
}

// TestNoRoute pins a message whose rules select no target: held with the
// reason, exit 64, and released once a rule selects a target.
func TestNoRoute(t *testing.T) {
	t.Run("no-route-held", func(t *testing.T) {
		c := newSpoolCase(t)
		c.config = routeRootToA
		code, inv := c.sendInput("To: alice\nSubject: x\n\nb\n")
		if code != 64 || len(c.service.got("a"))+len(c.service.got("b")) != 0 {
			t.Fatalf("Run() = %d, want 64 and nothing sent; output:\n%s", code, inv.output())
		}
		if e := c.entry(spool.HoldDir); e.Reason != reasonNoRoute {
			t.Errorf("reason %q, want %q", e.Reason, reasonNoRoute)
		}
		if !strings.Contains(inv.output(), `level=WARN msg="no route for message"`) || !strings.Contains(inv.output(), `level=DEBUG msg="message routed" targets=[]`) {
			t.Errorf("output:\n%s", inv.output())
		}
		t.Run("repeated-run-without-warning", func(t *testing.T) {
			code, inv := c.queueRun(serviceCaller)
			if code != 0 || strings.Contains(inv.output(), "WARN") || !strings.Contains(inv.output(), `level=DEBUG msg="no route for message"`) {
				t.Errorf("-q = %d; output:\n%s", code, inv.output())
			}
			if len(c.ids(spool.HoldDir)) != 1 {
				t.Errorf("hold %v, want the entry kept", c.ids(spool.HoldDir))
			}
		})
		t.Run("catch-all-releases", func(t *testing.T) {
			c.config = routeRootToA + catchAllToB
			code, inv := c.queueRun(serviceCaller)
			if code != 0 || !slices.Equal(c.service.got("b"), []string{"x"}) || len(c.service.got("a")) != 0 || !c.isSpoolEmpty() {
				t.Errorf("-q = %d, a %v, b %v; output:\n%s", code, c.service.got("a"), c.service.got("b"), inv.output())
			}
		})
	})
	t.Run("no-route-expires", func(t *testing.T) {
		c := newSpoolCase(t)
		c.config = routeRootToA
		c.sendInput("To: alice\nSubject: x\n\nb\n")
		c.clock.advance(7*24*time.Hour + time.Second)
		c.queueRun(serviceCaller)
		if len(c.ids(spool.HoldDir)) != 0 || c.entry(spool.FailedDir).Reason != reasonExpired {
			t.Errorf("hold %v, failed %v, want the entry expired", c.ids(spool.HoldDir), c.ids(spool.FailedDir))
		}
	})
	t.Run("no-route-spool-off", func(t *testing.T) {
		c := newSpoolCase(t)
		c.config = routeRootToA
		c.dir = ""
		code, inv := c.sendInput("To: alice\nSubject: x\n\nb\n")
		if code != 64 || !strings.Contains(inv.output(), `level=ERROR msg="message lost, spool off"`) {
			t.Errorf("Run() = %d; output:\n%s", code, inv.output())
		}
	})
	t.Run("unrouted-recipient-warned", func(t *testing.T) {
		c := newSpoolCase(t)
		c.config = routeRootToA
		code, inv := c.sendInput("To: root, alice, bob\nSubject: x\n\nb\n")
		if code != 0 || !slices.Equal(c.service.got("a"), []string{"x"}) || len(c.service.got("b")) != 0 {
			t.Fatalf("Run() = %d, a %v, b %v; output:\n%s", code, c.service.got("a"), c.service.got("b"), inv.output())
		}
		if !strings.Contains(inv.output(), `level=WARN msg="no route for recipient" unrouted=2`) || strings.Contains(inv.output(), "alice") {
			t.Errorf("output:\n%s", inv.output())
		}
	})
	t.Run("display-name-and-local-name", func(t *testing.T) {
		c := newSpoolCase(t)
		c.config = twoTargets + "\n[[route]]\nrecipient_regex = '^root(@|$)'\ntargets = [\"a\"]\n"
		code, _ := c.sendInput("Subject: x\n\nb\n", "Root <root@example.org>")
		code2, _ := c.sendInput("Subject: y\n\nb\n", "root")
		if code != 0 || code2 != 0 || !slices.Equal(c.service.got("a"), []string{"x", "y"}) {
			t.Errorf("Run() = %d, %d, a %v", code, code2, c.service.got("a"))
		}
	})
}

// TestRunWithoutRecipientsRouted pins the no-recipient policy with
// routes: a catch-all delivers, otherwise the message is held with 64.
func TestRunWithoutRecipientsRouted(t *testing.T) {
	t.Run("T-MTA-18/catch-all-delivers", noRecipientCase{nil, "Subject: t\n\nb\n", true}.check)
	t.Run("T-MTA-18/no-catch-all-holds", noRecipientCase{nil, "Subject: t\n\nb\n", false}.check)
	t.Run("T-MTA-19/catch-all-delivers", noRecipientCase{[]string{"-t"}, "Subject: t\n\nb\n", true}.check)
	t.Run("T-MTA-19/no-catch-all-holds", noRecipientCase{[]string{"-t"}, "Subject: t\n\nb\n", false}.check)
	t.Run("T-ADJ-08/catch-all-delivers", noRecipientCase{[]string{"-t"}, "b\n", true}.check)
	t.Run("T-ADJ-08/no-catch-all-holds", noRecipientCase{[]string{"-t"}, "b\n", false}.check)
}

type noRecipientCase struct {
	args       []string
	input      string
	isCatchAll bool
}

func (tc noRecipientCase) check(t *testing.T) {
	t.Helper()
	c := newSpoolCase(t)
	c.config = routeRootToA
	if tc.isCatchAll {
		c.config += catchAllToB
	}
	args := tc.args
	if args == nil {
		args = []string{}
	}
	code, inv := c.sendInput(tc.input, args...)
	switch {
	case tc.isCatchAll && (code != 0 || len(c.service.got("b")) != 1 || len(c.service.got("a")) != 0):
		t.Errorf("Run() = %d, a %v, b %v, want 0 and b only; output:\n%s", code, c.service.got("a"), c.service.got("b"), inv.output())
	case !tc.isCatchAll && (code != 64 || len(c.ids(spool.HoldDir)) != 1):
		t.Errorf("Run() = %d, hold %v, want 64 and the message held; output:\n%s", code, c.ids(spool.HoldDir), inv.output())
	}
}

// TestHeldRouted pins that a message held while the configuration was
// rejected is routed when it is released, in this spool and from the
// default one.
func TestHeldRouted(t *testing.T) {
	const rejected = "[target.a]\ntype = \"http\"\n"
	t.Run("released-to-routed-targets", func(t *testing.T) {
		c := newSpoolCase(t)
		c.config = rejected
		c.sendInput("To: root\nSubject: x\n\nb\n")
		c.config = routeRootToA
		code, inv := c.queueRun(serviceCaller)
		if code != 0 || !slices.Equal(c.service.got("a"), []string{"x"}) || len(c.service.got("b")) != 0 || !c.isSpoolEmpty() {
			t.Errorf("-q = %d, a %v, b %v; output:\n%s", code, c.service.got("a"), c.service.got("b"), inv.output())
		}
	})
	t.Run("suppressed-on-release", func(t *testing.T) {
		c := newSpoolCase(t)
		c.config = rejected
		c.sendInput("To: root\nSubject: Anacron\n\nb\n")
		c.config = twoTargets + "\n[[suppress]]\nsubject = \"anacron\"\n"
		code, inv := c.queueRun(serviceCaller)
		if code != 0 || len(c.service.got("a"))+len(c.service.got("b")) != 0 || !c.isSpoolEmpty() ||
			!strings.Contains(inv.output(), `msg="message suppressed" id=`) {
			t.Errorf("-q = %d; output:\n%s", code, inv.output())
		}
	})
	t.Run("config-reason-becomes-no-route", func(t *testing.T) {
		c := newSpoolCase(t)
		c.config = rejected
		c.sendInput("To: alice\nSubject: x\n\nb\n")
		c.config = routeRootToA
		code, inv := c.queueRun(serviceCaller)
		if code != 0 || c.entry(spool.HoldDir).Reason != reasonNoRoute || !strings.Contains(inv.output(), `level=WARN msg="no route for message"`) {
			t.Errorf("-q = %d; output:\n%s", code, inv.output())
		}
	})
	t.Run("queued-targets-fixed", func(t *testing.T) {
		c := newSpoolCase(t)
		c.config = routeRootToA
		c.service.reply("a", delivery.Temp)
		c.sendInput("To: root\nSubject: x\n\nb\n")
		c.config = twoTargets + "\n[[route]]\nrecipient = \"root\"\ntargets = [\"b\"]\n"
		c.clock.advance(2 * time.Minute)
		c.queueRun(serviceCaller)
		if !slices.Equal(c.service.got("a"), []string{"x", "x"}) || len(c.service.got("b")) != 0 || !c.isSpoolEmpty() {
			t.Errorf("a %v, b %v, want the queued entry retried for a only", c.service.got("a"), c.service.got("b"))
		}
	})
}

// TestReleaseOtherHoldRouted pins the three outcomes of the rules for an
// entry held in the default directory.
func TestReleaseOtherHoldRouted(t *testing.T) {
	cases := []struct {
		name, rules, input string
		area, reason       string
		got                []string
	}{
		{"routed", routeRootToA, "To: root\nSubject: x\n\nb\n", "", "", []string{"x"}},
		{"suppressed", twoTargets + "\n[[suppress]]\nsubject = \"x\"\n", "To: root\nSubject: x\n\nb\n", "", "", nil},
		{"no-route", routeRootToA, "To: alice\nSubject: x\n\nb\n", spool.HoldDir, reasonNoRoute, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newSpoolCase(t)
			own := t.TempDir()
			c.config = "[spool]\ndir = \"" + own + "\"\n\n[target.a]\ntype = \"http\"\n"
			c.sendInput(tc.input)
			c.config = "[spool]\ndir = \"" + own + "\"\n\n" + tc.rules
			code, inv := c.queueRun(serviceCaller)
			if code != 0 || len(c.ids(spool.HoldDir)) != 0 || !slices.Equal(c.service.got("a"), tc.got) || len(c.service.got("b")) != 0 {
				t.Fatalf("-q = %d, default hold %v, a %v, b %v; output:\n%s", code, c.ids(spool.HoldDir), c.service.got("a"), c.service.got("b"), inv.output())
			}
			sp, err := spool.Open(own)
			if err != nil {
				t.Fatal(err)
			}
			for _, area := range []string{spool.QueueDir, spool.HoldDir, spool.FailedDir} {
				ids, _ := sp.List(area)
				want := 0
				if area == tc.area {
					want = 1
				}
				if len(ids) != want {
					t.Errorf("own %s/ = %v, want %d entries", area, ids, want)
					continue
				}
				if want == 1 {
					if e, _ := sp.Peek(area, ids[0]); e.Reason != tc.reason {
						t.Errorf("reason %q, want %q", e.Reason, tc.reason)
					}
				}
			}
		})
	}
}

// TestRouteHeldWithoutRules pins that without rules a held message is
// released to every target without being read, so that one that cannot
// be read leaves hold/ as it did before there were rules.
func TestRouteHeldWithoutRules(t *testing.T) {
	q := &queue{router: route.New(nil, nil), targets: map[string]delivery.Target{"b": {ID: "b"}, "a": {ID: "a"}}}
	names, v, err := q.routeHeld(nil, &spool.Entry{}, func() ([]byte, error) { return nil, errors.New("unreadable") })
	if err != nil || v != deliverTo || !slices.Equal(names, []string{"a", "b"}) {
		t.Errorf("routeHeld() = %v, %v, %v; want every target", names, v, err)
	}
}

// TestHeldUnreadable pins a held message that the spool cannot read while
// rules need its subject: it stays in hold/ with an error, and -q exits 74.
func TestHeldUnreadable(t *testing.T) {
	c := newSpoolCase(t)
	c.config = "[target.a]\ntype = \"http\"\n"
	c.sendInput("To: root\nSubject: x\n\nb\n")
	id := c.ids(spool.HoldDir)[0]
	// A directory opens and locks like the file, and every read fails.
	eml := filepath.Join(c.dir, spool.HoldDir, id+".eml")
	if err := os.Remove(eml); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(eml, 0o770); err != nil {
		t.Fatal(err)
	}
	c.config = routeRootToA
	code, inv := c.queueRun(serviceCaller)
	if code != 74 || !strings.Contains(inv.output(), `level=ERROR msg="held message unreadable" id=`+id) ||
		len(c.service.got("a"))+len(c.service.got("b")) != 0 || len(c.ids(spool.HoldDir)) != 1 {
		t.Errorf("-q = %d, hold %v; output:\n%s", code, c.ids(spool.HoldDir), inv.output())
	}
}

// TestRouteHeldReadError pins that routeHeld tells an error of reading the
// spool from one of parsing, which release counts differently.
func TestRouteHeldReadError(t *testing.T) {
	q := &queue{router: newRouter(&config.Config{Routes: []config.Route{{Targets: []string{"a"}}}}), targets: map[string]delivery.Target{"a": {ID: "a"}}}
	_, _, err := q.routeHeld(nil, &spool.Entry{}, func() ([]byte, error) { return nil, errors.New("EIO") })
	if !errors.Is(err, errHeldRead) {
		t.Errorf("routeHeld() error = %v, want %v", err, errHeldRead)
	}
}

// TestReleaseKeepsHeldCopy pins that a held entry of the default spool
// whose copy a run killed before the removal already put into hold/ here
// is only removed there, and the copy stays.
func TestReleaseKeepsHeldCopy(t *testing.T) {
	c := newSpoolCase(t)
	own := t.TempDir()
	c.config = "[spool]\ndir = \"" + own + "\"\n\n[target.a]\ntype = \"http\"\n"
	c.sendInput("To: alice\nSubject: x\n\nb\n")
	def, err := spool.Open(c.dir)
	if err != nil {
		t.Fatal(err)
	}
	id := c.ids(spool.HoldDir)[0]
	rec, err := def.Lock(spool.HoldDir, id)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := rec.Message()
	entry := *rec.Entry
	_ = rec.Close()
	sp, err := spool.Open(own)
	if err != nil {
		t.Fatal(err)
	}
	entry.Reason = reasonNoRoute
	copied, err := sp.Create(spool.HoldDir, &entry, raw, spool.Quota{})
	if err != nil {
		t.Fatal(err)
	}
	_ = copied.Close()
	c.config = "[spool]\ndir = \"" + own + "\"\n\n" + routeRootToA
	code, inv := c.queueRun(serviceCaller)
	if code != 0 || !strings.Contains(inv.output(), `msg="held message already released" id=`+id) || strings.Contains(inv.output(), "WARN") {
		t.Fatalf("-q = %d; output:\n%s", code, inv.output())
	}
	if held, _ := sp.List(spool.HoldDir); len(c.ids(spool.HoldDir)) != 0 || !slices.Equal(held, []string{id}) {
		t.Errorf("default hold/ %v, own hold/ %v, want the copy here only", c.ids(spool.HoldDir), held)
	}
}

// TestDirectWarningsOnceForHeld pins that the warnings about direct
// addresses come when the message is received, and a queue run that
// routes the held message again logs them at debug.
func TestDirectWarningsOnceForHeld(t *testing.T) {
	c := newSpoolCase(t)
	c.config = "[general]\ntelegram_direct = \"tg\"\ntelegram_direct_chats = [1234]\n\n" +
		"[target.tg]\ntype = \"telegram\"\ntoken = \"1:a\"\nchat_id = 1\n\n" + twoTargets +
		"\n[[route]]\nrecipient = \"backup\"\ntargets = [\"a\"]\n"
	code, inv := c.sendInput("Subject: x\n\nb\n", "abc@telegram", "999@telegram")
	if code != 64 || !strings.Contains(inv.output(), `level=WARN msg="direct address invalid" count=1`) ||
		!strings.Contains(inv.output(), `level=WARN msg="direct chat not allowed" count=1`) {
		t.Fatalf("Run() = %d; output:\n%s", code, inv.output())
	}
	code, inv = c.queueRun(serviceCaller)
	if code != 0 || strings.Contains(inv.output(), "WARN") ||
		!strings.Contains(inv.output(), `level=DEBUG msg="direct address invalid"`) ||
		!strings.Contains(inv.output(), `level=DEBUG msg="direct chat not allowed"`) {
		t.Errorf("-q = %d; output:\n%s", code, inv.output())
	}
}
