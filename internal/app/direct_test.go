package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/6RUN0/mailcrier/internal/spool"
)

// botRequest is one sendMessage request the fake Bot API received.
type botRequest struct {
	token, chat         string
	isSilent, hasThread bool
}

// fakeBot answers the Bot API and the webhook of target mm: every request
// succeeds unless fail names its chat.
type fakeBot struct {
	mu       sync.Mutex
	requests []botRequest
	webhooks int
	fail     map[string]int
	server   *httptest.Server
}

func newFakeBot(t *testing.T) *fakeBot {
	t.Helper()
	bot := &fakeBot{fail: map[string]int{}}
	bot.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bot.mu.Lock()
		defer bot.mu.Unlock()
		if r.URL.Path == "/mm" {
			bot.webhooks++
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		token := strings.TrimPrefix(strings.TrimSuffix(r.URL.Path, "/sendMessage"), "/bot")
		chat, _ := body["chat_id"].(string)
		_, hasThread := body["message_thread_id"]
		silent, _ := body["disable_notification"].(bool)
		bot.requests = append(bot.requests, botRequest{token: token, chat: chat, isSilent: silent, hasThread: hasThread})
		if bot.fail[chat] > 0 {
			bot.fail[chat]--
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"ok":false,"error_code":503,"description":"unavailable"}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
	}))
	t.Cleanup(bot.server.Close)
	return bot
}

// chats returns the chats sendMessage went to, in order.
func (b *fakeBot) chats() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var chats []string
	for _, r := range b.requests {
		chats = append(chats, r.chat)
	}
	return chats
}

// webhookCount returns the requests of target mm.
func (b *fakeBot) webhookCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.webhooks
}

// directConfig declares the base telegram target tg, in chat 777 and topic
// 5, the webhook mm, and the direct chats; rest is appended.
func (b *fakeBot) directConfig(general, rest string) string {
	return "[general]\n" + general + "\n" +
		"[target.tg]\ntype = \"telegram\"\ntoken = \"1:TOKEN\"\nchat_id = 777\nmessage_thread_id = 5\ndisable_notification = true\n\n" +
		"[target.mm]\ntype = \"http\"\npreset = \"generic-json\"\nurl = \"" + b.server.URL + "/mm\"\n" + rest
}

// directGeneral turns direct chats on for tg.
const directGeneral = "telegram_direct = \"tg\"\ntelegram_direct_chats = [1234, -100123, \"@ops_channel\", 777]\n"

// run delivers input with args through the real targets.
func (b *fakeBot) run(t *testing.T, config, input string, args ...string) (int, *invocation) {
	t.Helper()
	inv := &invocation{config: config, args: args, stdin: strings.NewReader(input), client: rewritingClient(b.server)}
	return inv.run(t), inv
}

// TestDirectChats pins the recipients <chat>@telegram with telegram_direct.
func TestDirectChats(t *testing.T) {
	t.Run("T-ADJ-57/chat-without-topic", func(t *testing.T) {
		bot := newFakeBot(t)
		code, inv := bot.run(t, bot.directConfig(directGeneral, ""), "Subject: s\n\nb\n", "01234@telegram")
		if code != 0 || !slices.Equal(bot.chats(), []string{"1234"}) || bot.webhookCount() != 0 {
			t.Fatalf("Run() = %d, chats %v, webhooks %d; output:\n%s", code, bot.chats(), bot.webhookCount(), inv.output())
		}
		if r := bot.requests[0]; r.token != "1:TOKEN" || r.hasThread || !r.isSilent {
			t.Errorf("request %+v, want the token, no topic and silent", r)
		}
		if !strings.Contains(inv.output(), `msg="target delivered" target=1234@telegram`) {
			t.Errorf("output:\n%s", inv.output())
		}
	})
	t.Run("T-ADJ-57/blind-copy-to-chat", func(t *testing.T) {
		bot := newFakeBot(t)
		code, inv := bot.run(t, bot.directConfig(directGeneral, ""), "To: root\nBcc: @Ops_Channel@telegram\nSubject: s\n\nb\n", "-t")
		chats := bot.chats()
		slices.Sort(chats)
		if code != 0 || !slices.Equal(chats, []string{"777", "@ops_channel"}) || bot.webhookCount() != 1 {
			t.Errorf("Run() = %d, chats %v, webhooks %d; output:\n%s", code, chats, bot.webhookCount(), inv.output())
		}
	})
	t.Run("T-ADJ-57/plain-address-without-telegram-direct", func(t *testing.T) {
		bot := newFakeBot(t)
		code, inv := bot.run(t, bot.directConfig("", ""), "Subject: s\n\nb\n", "1234@telegram", "abc@telegram")
		if code != 0 || !slices.Equal(bot.chats(), []string{"777"}) || bot.webhookCount() != 1 || strings.Contains(inv.output(), "WARN") {
			t.Errorf("Run() = %d, chats %v, webhooks %d; output:\n%s", code, bot.chats(), bot.webhookCount(), inv.output())
		}
	})
	t.Run("T-ADJ-57/only-direct-despite-catch-all", func(t *testing.T) {
		bot := newFakeBot(t)
		code, inv := bot.run(t, bot.directConfig(directGeneral, "\n[[route]]\ntargets = [\"mm\"]\n"), "Subject: s\n\nb\n", "Ops <1234@TELEGRAM>")
		if code != 0 || !slices.Equal(bot.chats(), []string{"1234"}) || bot.webhookCount() != 0 {
			t.Errorf("Run() = %d, chats %v, webhooks %d; output:\n%s", code, bot.chats(), bot.webhookCount(), inv.output())
		}
	})
	for _, chatID := range []string{`"@OPS_Channel"`, `"01234"`} {
		t.Run("T-ADJ-57/chat-of-the-base-target-once/"+strings.Trim(chatID, `"@`), func(t *testing.T) {
			bot := newFakeBot(t)
			config := strings.Replace(bot.directConfig(directGeneral, "\n[[route]]\ntargets = [\"tg\"]\n"), "chat_id = 777", "chat_id = "+chatID, 1)
			direct := strings.ToLower(strings.Trim(chatID, `"`)) + "@telegram"
			code, inv := bot.run(t, config, "Subject: s\n\nb\n", "root", direct)
			if code != 0 || len(bot.chats()) != 1 || !bot.requests[0].hasThread {
				t.Errorf("Run() = %d, requests %+v, want one to the base target; output:\n%s", code, bot.requests, inv.output())
			}
		})
	}
	t.Run("T-ADJ-57/over-the-limit", func(t *testing.T) {
		bot := newFakeBot(t)
		code, inv := bot.run(t, bot.directConfig(directGeneral+"telegram_direct_max = 1\n", ""), "Subject: s\n\nb\n", "--", "1234@telegram", "-100123@telegram", "@ops_channel@telegram")
		if code != 0 || !slices.Equal(bot.chats(), []string{"1234"}) ||
			!strings.Contains(inv.output(), `level=WARN msg="direct chats over limit" count=2`) || strings.Contains(inv.output(), "100123") {
			t.Errorf("Run() = %d, chats %v; output:\n%s", code, bot.chats(), inv.output())
		}
	})
	t.Run("T-ADJ-57/invalid-and-not-allowed-are-plain", func(t *testing.T) {
		bot := newFakeBot(t)
		code, inv := bot.run(t, bot.directConfig(directGeneral, ""), "Subject: s\n\nb\n", "@telegram", "abc@telegram", "999@telegram")
		// The time of a record may end in .999.
		out := recordTime.ReplaceAllString(inv.output(), "")
		if code != 0 || !slices.Equal(bot.chats(), []string{"777"}) || bot.webhookCount() != 1 ||
			!strings.Contains(out, `level=WARN msg="direct address invalid" count=2`) || !strings.Contains(out, `level=WARN msg="direct chat not allowed" count=1`) ||
			strings.Contains(out, "999") || strings.Contains(out, "abc") {
			t.Errorf("Run() = %d, chats %v; output:\n%s", code, bot.chats(), out)
		}
	})
	t.Run("unrouted-recipient-beside-direct-chat", func(t *testing.T) {
		bot := newFakeBot(t)
		code, inv := bot.run(t, bot.directConfig(directGeneral, "\n[[route]]\nrecipient = \"backup\"\ntargets = [\"mm\"]\n"), "Subject: s\n\nb\n", "root", "1234@telegram")
		if code != 0 || !slices.Equal(bot.chats(), []string{"1234"}) || bot.webhookCount() != 0 ||
			!strings.Contains(inv.output(), `level=WARN msg="no route for recipient" count=1`) {
			t.Errorf("Run() = %d, chats %v; output:\n%s", code, bot.chats(), inv.output())
		}
	})
}

// directSpoolCase runs invocations through the fake Bot API against one
// spool directory and one clock.
type directSpoolCase struct {
	t      *testing.T
	bot    *fakeBot
	dir    string
	clock  *clock
	config string
}

func (c *directSpoolCase) run(input string, args ...string) (int, *invocation) {
	c.t.Helper()
	inv := &invocation{
		config: c.config, args: args, stdin: strings.NewReader(input), client: rewritingClient(c.bot.server),
		creds: elevatedUser, spoolDir: c.dir, now: c.clock.now,
	}
	if args == nil {
		inv.args, inv.creds = []string{"-q"}, serviceCaller
	}
	code := inv.run(c.t)
	c.clock.advance(time.Millisecond)
	return code, inv
}

// entry reads the sidecar of the only entry in area.
func (c *directSpoolCase) entry(area string) *spool.Entry {
	c.t.Helper()
	return (&spoolCase{t: c.t, dir: c.dir}).entry(area)
}

// TestDirectChatsQueued pins a direct chat in the spool: its name in the
// sidecar, a retry with the configuration of the moment, and its removal.
func TestDirectChatsQueued(t *testing.T) {
	setup := func(t *testing.T) *directSpoolCase {
		bot := newFakeBot(t)
		c := &directSpoolCase{t: t, bot: bot, dir: t.TempDir(), clock: newClock(), config: bot.directConfig(directGeneral, "")}
		bot.fail["1234"] = 1
		if code, inv := c.run("Subject: s\n\nb\n", "1234@telegram"); code != 0 {
			t.Fatalf("Run() = %d; output:\n%s", code, inv.output())
		}
		if e := c.entry(spool.QueueDir); e.Targets["1234@telegram"] == nil || len(e.Targets) != 1 {
			t.Fatalf("sidecar targets %v, want the direct chat", e.Targets)
		}
		c.clock.advance(2 * time.Minute)
		return c
	}
	t.Run("retry-takes-the-configuration-of-the-moment", func(t *testing.T) {
		c := setup(t)
		c.config = strings.Replace(c.config, "1:TOKEN", "2:OTHER", 1)
		code, inv := c.run("")
		last := c.bot.requests[len(c.bot.requests)-1]
		if code != 0 || last.token != "2:OTHER" || last.chat != "1234" || len((&spoolCase{t: t, dir: c.dir}).ids(spool.QueueDir)) != 0 {
			t.Errorf("-q = %d, last request %+v; output:\n%s", code, last, inv.output())
		}
	})
	t.Run("mailq-names-the-chat", func(t *testing.T) {
		c := setup(t)
		inv := &invocation{config: c.config, args: []string{"-bp"}, creds: serviceCaller, spoolDir: c.dir, now: c.clock.now}
		if code := inv.run(t); code != 0 || !strings.Contains(inv.stdout.String(), "  1234@telegram pending attempts=1") {
			t.Errorf("-bp = %d, stdout:\n%s", code, inv.stdout.String())
		}
	})
	for name, config := range map[string]func(string) string{
		"telegram-direct-removed": func(config string) string { return strings.Replace(config, directGeneral, "", 1) },
		"chat-removed-from-list":  func(config string) string { return strings.Replace(config, "[1234, ", "[", 1) },
	} {
		t.Run(name, func(t *testing.T) {
			c := setup(t)
			c.config = config(c.config)
			code, inv := c.run("")
			if code != 0 || len(c.bot.requests) != 1 || len((&spoolCase{t: t, dir: c.dir}).ids(spool.QueueDir)) != 0 ||
				!strings.Contains(inv.output(), `msg="target removed, message dropped for it" id=`) {
				t.Errorf("-q = %d, requests %+v; output:\n%s", code, c.bot.requests, inv.output())
			}
		})
	}
	t.Run("released-from-hold-with-direct-chats", func(t *testing.T) {
		bot := newFakeBot(t)
		c := &directSpoolCase{t: t, bot: bot, dir: t.TempDir(), clock: newClock(), config: "[target.x]\ntype = \"http\"\n"}
		if code, _ := c.run("Subject: s\n\nb\n", "root", "1234@telegram"); code != 78 {
			t.Fatalf("Run() = %d, want 78", code)
		}
		c.config = bot.directConfig(directGeneral, "\n[[route]]\nrecipient = \"root\"\ntargets = [\"mm\"]\n")
		code, inv := c.run("")
		if code != 0 || !slices.Equal(bot.chats(), []string{"1234"}) || bot.webhookCount() != 1 {
			t.Errorf("-q = %d, chats %v, webhooks %d; output:\n%s", code, bot.chats(), bot.webhookCount(), inv.output())
		}
	})
}

// recordTime is the time field of a log record, which tests drop before
// they look for a number that must not appear.
var recordTime = regexp.MustCompile(`time=\S+ `)
