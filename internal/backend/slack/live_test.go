package slack

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/6RUN0/mailcrier/internal/delivery"
	"github.com/6RUN0/mailcrier/internal/message"
	"github.com/6RUN0/mailcrier/internal/render"
	"github.com/6RUN0/mailcrier/internal/text"
)

// liveEnv names a file of KEY=VALUE lines with SLACK_BOT_TOKEN, the Bot
// User OAuth Token (xoxb-...) of a Slack app, and SLACK_CHANNEL, the id
// (C...) of a channel the bot is a member of (/invite @app in the
// channel). The bot token needs the scopes chat:write and files:write to
// send, channels:history (groups:history for a private channel) and
// files:read to read the message back. The live test posts into that
// channel. Without the file it is skipped, so that no run of the suite
// posts to Slack by accident. SLACK_API_URL, optional, replaces
// DefaultAPIURL.
const liveEnv = "MAILCRIER_SLACK_ENV"

// TestLiveSlack sends one message with mention syntax, mrkdwn characters
// and Cyrillic, and one file, through the built-in template and delivery
// to the real Web API, then reads both back: the newest message of the
// channel that carries the unique marker has the rendered text with the
// mentions still escaped, and the file shared in its thread has its name
// and size.
func TestLiveSlack(t *testing.T) {
	t.Run("T-ESC-08/live-text-and-file", func(t *testing.T) {
		live := newLiveAPI(t)
		marker := fmt.Sprintf("mailcrier-live-%d", time.Now().UnixNano())
		files := []message.Attachment{{Name: "report.log", ContentType: "text/plain", Data: []byte("mailcrier live test\nкириллица\n")}}
		d := render.Data{
			Subject:  "mailcrier live test " + marker + " <!here> *bold* ёжик",
			Hostname: "test.example.org",
			From:     message.Address{Name: "Cron <Daemon>", Addr: "root"},
			Body:     "<!channel> <@U123> <https://evil.example|click> x < y && z > 0\n" + strings.Repeat("кириллица & <tag> ", 10),
			Attachments: []render.Attachment{
				{Name: "report.log", ContentType: "text/plain", Size: int64(len(files[0].Data))},
			},
			Strings: render.DefaultStrings(),
		}
		tmpl, err := render.Builtin(text.FormatSlackMrkdwn)
		if err != nil {
			t.Fatal(err)
		}
		want, truncated, err := render.Fit(tmpl, d, maxText, text.UTF16Len)
		if err != nil || truncated {
			t.Fatalf("render: truncated %v, err %v", truncated, err)
		}
		for _, escaped := range []string{"&lt;!here&gt;", "&lt;!channel&gt;", "&lt;@U123&gt;", "&lt;https://evil.example|click&gt;"} {
			if !strings.Contains(want, escaped) {
				t.Fatalf("rendered text lacks %s: %q", escaped, want)
			}
		}
		result := delivery.Deliver(context.Background(), []delivery.Target{{ID: "sl", Sender: live.sender, Template: tmpl}}, d, files)[0]
		if result.Status != delivery.OK || result.Err != nil {
			t.Fatalf("result: status %v, err %v", result.Status, result.Err)
		}
		posted := live.findMessage(t, marker)
		if posted.TS != live.capture.ts {
			t.Errorf("message with the marker has ts %q, chat.postMessage answered %q", posted.TS, live.capture.ts)
		}
		if strings.TrimSpace(posted.Text) != strings.TrimSpace(want) {
			t.Errorf("stored text differs from the rendered text:\n got %q\nwant %q", posted.Text, want)
		}
		file := live.fileInfo(t)
		if file.Name != "report.log" || file.Size != int64(len(files[0].Data)) {
			t.Errorf("file %q of %d bytes, want report.log of %d bytes", file.Name, file.Size, len(files[0].Data))
		}
		shares := append(file.Shares.Public[live.capture.channel], file.Shares.Private[live.capture.channel]...)
		if len(shares) == 0 {
			t.Logf("files.info lists no share in the channel yet; Slack shares a file asynchronously")
		}
		for _, share := range shares {
			if share.ThreadTS != live.capture.ts {
				t.Errorf("file shared in thread %q, want the thread of the message %q", share.ThreadTS, live.capture.ts)
			}
		}
	})
}

// liveAPI is a sender to the channel of liveEnv whose client keeps the
// channel and ts of the posted message and the id of the uploaded file.
type liveAPI struct {
	token   string
	channel string
	apiURL  string
	sender  *Sender
	client  *http.Client
	capture *postCapture
}

// newLiveAPI returns the Web API of liveEnv, or skips the test when
// liveEnv is not set.
func newLiveAPI(t *testing.T) *liveAPI {
	t.Helper()
	path := os.Getenv(liveEnv)
	if path == "" {
		t.Skip(liveEnv + " is not set")
	}
	env := readEnvFile(t, path)
	token, channel := env["SLACK_BOT_TOKEN"], env["SLACK_CHANNEL"]
	if token == "" || channel == "" {
		t.Fatalf("%s lacks SLACK_BOT_TOKEN or SLACK_CHANNEL", path)
	}
	apiURL := env["SLACK_API_URL"]
	if apiURL == "" {
		apiURL = DefaultAPIURL
	}
	capture := &postCapture{base: http.DefaultTransport}
	return &liveAPI{
		token: token, channel: channel, apiURL: apiURL, capture: capture,
		client: &http.Client{Timeout: 15 * time.Second},
		sender: New(Options{Token: token, Channel: channel, APIURL: apiURL, Client: &http.Client{Timeout: 15 * time.Second, Transport: capture}}),
	}
}

// historyMessage is the part of a message of conversations.history the
// live test reads.
type historyMessage struct {
	TS   string `json:"ts"`
	Text string `json:"text"`
}

// findMessage returns the newest of the last 50 messages of the channel
// whose text carries marker.
func (a *liveAPI) findMessage(t *testing.T, marker string) historyMessage {
	t.Helper()
	var answer struct {
		Messages []historyMessage `json:"messages"`
	}
	a.get(t, "conversations.history", url.Values{"channel": {a.channel}, "limit": {"50"}}, &answer)
	for _, msg := range answer.Messages {
		if strings.Contains(msg.Text, marker) {
			return msg
		}
	}
	t.Fatalf("none of the %d newest messages carries the marker %s", len(answer.Messages), marker)
	return historyMessage{}
}

// share is one share of a file in files.info.
type share struct {
	TS       string `json:"ts"`
	ThreadTS string `json:"thread_ts"`
}

// liveFile is the part of the file object of files.info the live test
// reads.
type liveFile struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	Shares struct {
		Public  map[string][]share `json:"public"`
		Private map[string][]share `json:"private"`
	} `json:"shares"`
}

// fileInfo returns the file uploaded last, by its id from the answer of
// files.getUploadURLExternal.
func (a *liveAPI) fileInfo(t *testing.T) liveFile {
	t.Helper()
	if a.capture.fileID == "" {
		t.Fatal("files.getUploadURLExternal answered without a file id")
	}
	var answer struct {
		File liveFile `json:"file"`
	}
	a.get(t, "files.info", url.Values{"file": {a.capture.fileID}}, &answer)
	return answer.File
}

// get calls a Web API method by GET and decodes its answer into into; an
// answer with ok false fails the test with its error code.
func (a *liveAPI) get(t *testing.T, method string, query url.Values, into any) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, a.apiURL+"/"+method+"?"+query.Encode(), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	resp, err := a.client.Do(req)
	if err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	var status answer
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &status) != nil || !status.OK {
		t.Fatalf("%s: status %d, error %q", method, resp.StatusCode, status.Error)
	}
	if err := json.Unmarshal(body, into); err != nil {
		t.Fatalf("%s: %v", method, err)
	}
}

// postCapture is a RoundTripper that keeps the channel and ts of the
// answer of chat.postMessage and the file id of the answer of
// files.getUploadURLExternal.
type postCapture struct {
	base http.RoundTripper

	mu      sync.Mutex
	channel string
	ts      string
	fileID  string
}

// RoundTrip performs req and reads the fields from a 2xx answer of the two
// methods, handing the body on unchanged.
func (c *postCapture) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := c.base.RoundTrip(req)
	if err != nil || resp.StatusCode/100 != 2 {
		return resp, err
	}
	isPost := strings.HasSuffix(req.URL.Path, "/chat.postMessage")
	isUpload := strings.HasSuffix(req.URL.Path, "/files.getUploadURLExternal")
	if !isPost && !isUpload {
		return resp, nil
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("read answer: %w", err)
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	var got answer
	if json.Unmarshal(body, &got) == nil && got.OK {
		c.mu.Lock()
		if isPost {
			c.channel, c.ts = got.Channel, got.TS
		} else {
			c.fileID = got.FileID
		}
		c.mu.Unlock()
	}
	return resp, nil
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
