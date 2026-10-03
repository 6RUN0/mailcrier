package telegram

import (
	"bufio"
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/6RUN0/mailcrier/internal/backend"
	"github.com/6RUN0/mailcrier/internal/delivery"
	"github.com/6RUN0/mailcrier/internal/message"
	"github.com/6RUN0/mailcrier/internal/render"
	"github.com/6RUN0/mailcrier/internal/text"
)

// liveEnv names a file of KEY=VALUE lines with TELEGRAM_BOT_TOKEN and
// TELEGRAM_CHAT_ID of a test bot. Without it the live test is skipped, so
// that no run of the suite sends to Telegram by accident.
const liveEnv = "SLENDMAIL_TELEGRAM_ENV"

// TestLiveTelegram sends one message with markup characters, Cyrillic and
// a URL, and one document, to the real Bot API, which checks
// the HTML parse mode and link_preview_options for real.
func TestLiveTelegram(t *testing.T) {
	sender := liveSender(t)
	files := []message.Attachment{{Name: "report.log", ContentType: "text/plain", Data: []byte("slendmail live test\n")}}
	d := render.Data{
		Subject:  "slendmail live test <b> & ёжик",
		Hostname: "test.example.org",
		From:     message.Address{Name: "Cron <Daemon>", Addr: "root"},
		Body:     "x < y && z > 0\nhttps://example.org/\n" + strings.Repeat("кириллица & <tag> ", 20),
		Attachments: []render.Attachment{
			{Name: "report.log", ContentType: "text/plain", Size: int64(len(files[0].Data))},
		},
	}
	result := deliverTo(t, sender, d, files)
	if result.Status != delivery.OK || result.Err != nil {
		t.Fatalf("result: status %v, err %v", result.Status, result.Err)
	}
}

// TestLiveTelegramLongText sends a text over the limit with on_long
// blockquote to the real Bot API: the cut body in an expandable blockquote
// by sendMessage, then the full text as message.txt, which checks the
// markup of the collapsed block and the cut for real.
func TestLiveTelegramLongText(t *testing.T) {
	sender := liveSender(t)
	tmpl, err := render.Builtin(text.FormatTelegramHTML)
	if err != nil {
		t.Fatal(err)
	}
	d := render.Data{
		Subject: "slendmail live test: long text", Hostname: "test.example.org", From: message.Address{Addr: "root"},
		Body: strings.Repeat("line of a long report with <markup> & ёжик 😀\n", 200), Strings: render.DefaultStrings(),
	}
	target := delivery.Target{ID: "tg", Sender: sender, Template: tmpl, OnLong: delivery.OnLongBlockquote}
	result := delivery.Deliver(context.Background(), []delivery.Target{target}, d, nil)[0]
	if result.Status != delivery.OK || result.Err != nil || !result.IsTruncated || result.TextRejected != nil {
		t.Fatalf("result: status %v, err %v, truncated %v, text rejected %v", result.Status, result.Err, result.IsTruncated, result.TextRejected)
	}
}

// TestLiveTelegramLimitUnit pins the unit of the 4096 limit of sendMessage
// against the real Bot API: 4096 characters outside the BMP, 8192 UTF-16
// units, are accepted, and 4097 of them are rejected. The limit thus
// counts characters, as Caps.Measure does.
func TestLiveTelegramLimitUnit(t *testing.T) {
	t.Run("T-LIM-03/limit-counts-characters", func(t *testing.T) {
		sender := liveSender(t)
		atLimit := strings.Repeat("😀", maxText)
		if err := sender.Send(context.Background(), backend.Payload{Text: atLimit}); err != nil {
			t.Fatalf("%d characters outside the BMP: %v", maxText, err)
		}
		err := sender.Send(context.Background(), backend.Payload{Text: atLimit + "😀"})
		var deliveryErr *backend.Error
		if !errors.As(err, &deliveryErr) || deliveryErr.Status != http.StatusBadRequest {
			t.Fatalf("%d characters outside the BMP: err = %v, want 400", maxText+1, err)
		}
		if n := sender.Caps().Measure(atLimit); n != maxText {
			t.Errorf("Measure = %d, want %d", n, maxText)
		}
	})
}

// liveSender returns a sender to the chat of liveEnv, or skips the test
// when liveEnv is not set.
func liveSender(t *testing.T) *Sender {
	t.Helper()
	path := os.Getenv(liveEnv)
	if path == "" {
		t.Skip(liveEnv + " is not set")
	}
	env := readEnvFile(t, path)
	token, chatID := env["TELEGRAM_BOT_TOKEN"], env["TELEGRAM_CHAT_ID"]
	if token == "" || chatID == "" {
		t.Fatalf("%s lacks TELEGRAM_BOT_TOKEN or TELEGRAM_CHAT_ID", path)
	}
	return New(Options{Token: token, ChatID: chatID, DisableNotification: true, Client: &http.Client{Timeout: 15 * time.Second}})
}

// readEnvFile returns the KEY=VALUE lines of path; the values are never
// printed. A file that others than its owner may read fails the test: it
// holds a bot token.
func readEnvFile(t *testing.T, path string) map[string]string {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("%s has mode %v, want 0600", path, info.Mode().Perm())
	}
	env := map[string]string{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		key, value, ok := strings.Cut(strings.TrimSpace(scanner.Text()), "=")
		if ok && !strings.HasPrefix(key, "#") {
			env[strings.TrimSpace(key)] = strings.Trim(strings.TrimSpace(value), `"'`)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return env
}
