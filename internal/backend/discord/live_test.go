package discord

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/6RUN0/mailcrier/internal/backend"
	"github.com/6RUN0/mailcrier/internal/delivery"
	"github.com/6RUN0/mailcrier/internal/message"
	"github.com/6RUN0/mailcrier/internal/render"
	"github.com/6RUN0/mailcrier/internal/text"
)

// liveEnv names a file of KEY=VALUE lines with DISCORD_WEBHOOK_URL, the
// URL of a channel webhook: in Discord, the channel settings, Integrations,
// Webhooks, New Webhook, Copy Webhook URL. The live tests post into that
// channel. DISCORD_USER_ID, optional, is the id of a real member of the
// channel (Developer Mode, right click on the user, Copy User ID): the
// mention of a user who exists is the one that would resolve. Without the
// file the live tests are skipped, so that no run of the suite posts to
// Discord by accident.
const liveEnv = "MAILCRIER_DISCORD_ENV"

// liveMessage is the part of a Discord message object the live tests read.
type liveMessage struct {
	ID              string            `json:"id"`
	Content         string            `json:"content"`
	MentionEveryone bool              `json:"mention_everyone"`
	Mentions        []json.RawMessage `json:"mentions"`
	MentionRoles    []json.RawMessage `json:"mention_roles"`
	Attachments     []struct {
		Filename string `json:"filename"`
		Size     int64  `json:"size"`
	} `json:"attachments"`
}

// TestLiveDiscord sends one message with markup characters, Cyrillic,
// @everyone and a user mention, and one file, through the built-in
// template and delivery to the real webhook, then reads the stored message
// back: the content is the rendered text with its escaping intact, the
// file arrived whole, and no mention resolved.
func TestLiveDiscord(t *testing.T) {
	t.Run("T-ESC-09/live-content-file-no-mentions", func(t *testing.T) {
		live := newLiveWebhook(t)
		files := []message.Attachment{{Name: "report.log", ContentType: "text/plain", Data: []byte("mailcrier live test\nкириллица\n")}}
		d := render.Data{
			Subject:  "mailcrier live test **bold** _it_ ~~s~~ ||sp|| ёжик @everyone",
			Hostname: "test.example.org",
			From:     message.Address{Name: "Cron <Daemon>", Addr: "root"},
			Body:     "@everyone @here " + live.mention + " x < y && z > 0\n```\n# not a heading\n" + strings.Repeat("кириллица & <tag> ", 10),
			Attachments: []render.Attachment{
				{Name: "report.log", ContentType: "text/plain", Size: int64(len(files[0].Data))},
			},
			Strings: render.DefaultStrings(),
		}
		tmpl, err := render.Builtin(text.FormatDiscord)
		if err != nil {
			t.Fatal(err)
		}
		want, truncated, err := render.Fit(tmpl, d, maxText, utf8.RuneCountInString)
		if err != nil || truncated {
			t.Fatalf("render: truncated %v, err %v", truncated, err)
		}
		for _, escaped := range []string{`\*\*bold\*\*`, `\_it\_`, `\~\~s\~\~`, `\|\|sp\|\|`} {
			if !strings.Contains(want, escaped) {
				t.Fatalf("rendered text lacks %s: %q", escaped, want)
			}
		}
		result := delivery.Deliver(context.Background(), []delivery.Target{{ID: "dc", Sender: live.sender, Template: tmpl}}, d, files)[0]
		if result.Status != delivery.OK || result.Err != nil {
			t.Fatalf("result: status %v, err %v", result.Status, result.Err)
		}
		got := live.fetch(t)
		if strings.TrimSpace(got.Content) != strings.TrimSpace(want) {
			t.Errorf("stored content differs from the rendered text:\n got %q\nwant %q", got.Content, want)
		}
		if len(got.Attachments) != 1 || got.Attachments[0].Filename != "report.log" || got.Attachments[0].Size != int64(len(files[0].Data)) {
			t.Errorf("attachments = %+v, want report.log of %d bytes", got.Attachments, len(files[0].Data))
		}
		if got.MentionEveryone || len(got.Mentions) != 0 || len(got.MentionRoles) != 0 {
			t.Errorf("mentions resolved: mention_everyone %v, mentions %s, mention_roles %s", got.MentionEveryone, got.Mentions, got.MentionRoles)
		}
	})
}

// TestLiveDiscordLimitUnit pins the unit of the 2000 limit of content
// against the real webhook: 2000 characters outside the BMP, 4000 UTF-16
// units, are accepted, and 2001 are rejected with 400. The limit thus
// counts characters, as Caps.Measure does.
func TestLiveDiscordLimitUnit(t *testing.T) {
	live := newLiveWebhook(t)
	atLimit := strings.Repeat("😀", maxText)
	if caps := live.sender.Caps(); caps.Measure != nil || caps.MaxText != maxText {
		t.Fatalf("Caps count the limit %d with Measure %p, want %d characters", caps.MaxText, caps.Measure, maxText)
	}
	if err := live.sender.Send(context.Background(), backend.Payload{Text: atLimit}); err != nil {
		t.Fatalf("%d characters outside the BMP: %v", maxText, err)
	}
	err := live.sender.Send(context.Background(), backend.Payload{Text: atLimit + "😀"})
	var deliveryErr *backend.Error
	if !errors.As(err, &deliveryErr) || deliveryErr.Status != http.StatusBadRequest {
		t.Fatalf("%d characters outside the BMP: err = %v, want 400", maxText+1, err)
	}
}

// liveWebhook is a sender to the webhook of liveEnv whose client keeps
// the id of the last message the webhook answered with.
type liveWebhook struct {
	url     string
	mention string
	sender  *Sender
	client  *http.Client
	capture *idCapture
}

// newLiveWebhook returns the webhook of liveEnv, or skips the test when
// liveEnv is not set.
func newLiveWebhook(t *testing.T) *liveWebhook {
	t.Helper()
	path := os.Getenv(liveEnv)
	if path == "" {
		t.Skip(liveEnv + " is not set")
	}
	env := readEnvFile(t, path)
	webhook := env["DISCORD_WEBHOOK_URL"]
	if webhook == "" {
		t.Fatalf("%s lacks DISCORD_WEBHOOK_URL", path)
	}
	mention := "<@123>"
	if id := env["DISCORD_USER_ID"]; id != "" {
		mention = "<@" + id + ">"
	}
	capture := &idCapture{base: http.DefaultTransport}
	client := &http.Client{Timeout: 15 * time.Second, Transport: capture}
	return &liveWebhook{
		url: webhook, mention: mention, capture: capture, client: &http.Client{Timeout: 15 * time.Second},
		sender: New(Options{URL: webhook, Client: client}),
	}
}

// fetch reads back the last message the webhook stored, by
// GET <webhook>/messages/<id>; a thread_id of the URL is kept.
func (w *liveWebhook) fetch(t *testing.T) liveMessage {
	t.Helper()
	if w.capture.id == "" {
		t.Fatal("the webhook answered without a message id")
	}
	endpoint, err := url.Parse(w.url)
	if err != nil {
		t.Fatal(err)
	}
	endpoint.Path = strings.TrimSuffix(endpoint.Path, "/") + "/messages/" + url.PathEscape(w.capture.id)
	resp, err := w.client.Get(endpoint.String())
	if err != nil {
		t.Fatal(backend.WithoutURL(err))
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET message: status %d", resp.StatusCode)
	}
	var msg liveMessage
	if err := json.NewDecoder(resp.Body).Decode(&msg); err != nil {
		t.Fatal(err)
	}
	if msg.ID != w.capture.id {
		t.Fatalf("GET message: id %q, want %q", msg.ID, w.capture.id)
	}
	return msg
}

// idCapture is a RoundTripper that keeps the id of the message in a
// successful answer of the webhook, which wait=true makes Discord send.
type idCapture struct {
	base http.RoundTripper
	id   string
}

// RoundTrip performs req and reads the id from a 2xx answer, handing the
// body on unchanged.
func (c *idCapture) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := c.base.RoundTrip(req)
	if err != nil || resp.StatusCode/100 != 2 {
		return resp, err
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("read answer: %w", err)
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	var answer struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(body, &answer) == nil {
		c.id = answer.ID
	}
	return resp, nil
}

// readEnvFile returns the KEY=VALUE lines of path; the values are never
// printed. A file that others than its owner may read fails the test: it
// holds a webhook token.
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
