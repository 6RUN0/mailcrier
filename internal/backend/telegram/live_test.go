package telegram

import (
	"bufio"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/6RUN0/slendmail/internal/delivery"
	"github.com/6RUN0/slendmail/internal/message"
	"github.com/6RUN0/slendmail/internal/render"
)

// liveEnv names a file of KEY=VALUE lines with TELEGRAM_BOT_TOKEN and
// TELEGRAM_CHAT_ID of a test bot. Without it the live test is skipped, so
// that no run of the suite sends to Telegram by accident.
const liveEnv = "SLENDMAIL_TELEGRAM_ENV"

// TestLiveTelegram sends one message with markup characters, Cyrillic and
// a URL, and one document, to the real Bot API, which checks
// the HTML parse mode and link_preview_options for real.
func TestLiveTelegram(t *testing.T) {
	path := os.Getenv(liveEnv)
	if path == "" {
		t.Skip(liveEnv + " is not set")
	}
	env := readEnvFile(t, path)
	token, chatID := env["TELEGRAM_BOT_TOKEN"], env["TELEGRAM_CHAT_ID"]
	if token == "" || chatID == "" {
		t.Fatalf("%s lacks TELEGRAM_BOT_TOKEN or TELEGRAM_CHAT_ID", path)
	}
	sender := New(Options{Token: token, ChatID: chatID, DisableNotification: true, Client: &http.Client{Timeout: 15 * time.Second}})
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

// readEnvFile returns the KEY=VALUE lines of path; the values are never
// printed.
func readEnvFile(t *testing.T, path string) map[string]string {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
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
