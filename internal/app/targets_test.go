package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/6RUN0/slendmail/internal/backend"
	"github.com/6RUN0/slendmail/internal/config"
	"github.com/6RUN0/slendmail/internal/delivery"
	"github.com/6RUN0/slendmail/internal/redact"
)

// statusServer answers every request with status and counts the requests.
func statusServer(t *testing.T, status int, requests *atomic.Int32) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(status)
	}))
	t.Cleanup(server.Close)
	return server
}

// TestRunServiceStatuses pins the exit status for the answers of a
// service when it is the only target: without a queue a temporary failure
// loses the message like a permanent one, so both exit 69.
func TestRunServiceStatuses(t *testing.T) {
	t.Run("T-ADJ-27/rate-limited-without-spool", serviceStatusCase{httpTargetConfig, http.StatusTooManyRequests, "temp"}.check)
	t.Run("T-ADJ-27/internal-error-without-spool", serviceStatusCase{httpTargetConfig, http.StatusInternalServerError, "temp"}.check)
	t.Run("T-ADJ-27/bad-gateway-without-spool", serviceStatusCase{httpTargetConfig, http.StatusBadGateway, "temp"}.check)
	t.Run("T-ADJ-27/rejected", serviceStatusCase{httpTargetConfig, http.StatusBadRequest, "perm"}.check)
	t.Run("T-LIM-10/discord-rejected", serviceStatusCase{discordConfig, http.StatusNotFound, "perm"}.check)
	t.Run("T-LIM-10/discord-rate-limited-without-spool", serviceStatusCase{discordConfig, http.StatusTooManyRequests, "temp"}.check)
}

type serviceStatusCase struct {
	config    func(url string) string
	status    int
	wantClass string
}

func (tc serviceStatusCase) check(t *testing.T) {
	var requests atomic.Int32
	server := statusServer(t, tc.status, &requests)
	inv := &invocation{config: tc.config(server.URL), stdin: strings.NewReader("Subject: t\n\nb\n")}
	if code := inv.run(t); code != 69 {
		t.Fatalf("Run() = %d, want 69", code)
	}
	want := fmt.Sprintf("class=%s status=%d", tc.wantClass, tc.status)
	if log := inv.log("slendmail"); requests.Load() != 1 || !strings.Contains(log, want) {
		t.Errorf("%d requests, log lacks %q:\n%s", requests.Load(), want, log)
	}
}

// discordConfig configures one discord target with a webhook on url.
func discordConfig(url string) string {
	return "[target.dc]\ntype = \"discord\"\nurl = \"" + url + "/api/webhooks/1/T0KEN\"\n"
}

// rewritingClient sends every request to server over plain HTTP, keeping
// the path and query, so that a target with a fixed service URL reaches a
// local fake.
func rewritingClient(server *httptest.Server) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		local, err := url.Parse(server.URL)
		if err != nil {
			return nil, err
		}
		req = req.Clone(req.Context())
		req.URL.Scheme, req.URL.Host, req.Host = local.Scheme, local.Host, local.Host
		return http.DefaultTransport.RoundTrip(req)
	})}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// TestRunSeveralSlackTargets pins a configuration of several Slack
// targets, each with its own token and channel; " #alerts" reaches the
// API as "#alerts".
func TestRunSeveralSlackTargets(t *testing.T) {
	var mu sync.Mutex
	posts := map[string]string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg struct {
			Channel string `json:"channel"`
		}
		_ = json.NewDecoder(r.Body).Decode(&msg)
		mu.Lock()
		posts[msg.Channel] = r.Header.Get("Authorization")
		mu.Unlock()
		_, _ = io.WriteString(w, `{"ok":true,"channel":"C1","ts":"1.2"}`)
	}))
	defer server.Close()
	config := "[target.prod]\ntype = \"slack\"\ntoken = \"xoxb-prod\"\nchannel = \" #alerts\"\n\n" +
		"[target.dev]\ntype = \"slack\"\ntoken = \"xoxb-dev\"\nchannel = \"#dev-alerts\"\n"
	inv := &invocation{config: config, client: rewritingClient(server), stdin: strings.NewReader("Subject: t\n\nb\n")}
	if code := inv.run(t); code != 0 {
		t.Fatalf("Run() = %d, want 0; log:\n%s", code, inv.log("slendmail"))
	}
	if want := "map[#alerts:Bearer xoxb-prod #dev-alerts:Bearer xoxb-dev]"; fmt.Sprint(posts) != want {
		t.Errorf("posts = %v, want %s", posts, want)
	}
}

// TestRunWebhookPresets pins the http target presets with the options
// the payload formats of a webhook need: username and channel for
// Mattermost, extra headers for any receiver.
func TestRunWebhookPresets(t *testing.T) {
	var mu sync.Mutex
	requests := map[string]*http.Request{}
	bodies := map[string]map[string]any{}
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("%s: %v", r.URL.Path, err)
		}
		mu.Lock()
		requests[r.URL.Path], bodies[r.URL.Path] = r, body
		mu.Unlock()
	}))
	defer server.Close()
	config := "[target.mm]\ntype = \"http\"\npreset = \"mattermost\"\nurl = \"" + server.URL + "/mm\"\nusername = \"slendmail\"\nchannel = \"alerts\"\n\n" +
		"[target.sw]\ntype = \"http\"\npreset = \"slack-webhook\"\nurl = \"" + server.URL + "/sw\"\n\n" +
		"[target.api]\ntype = \"http\"\npreset = \"generic-json\"\nurl = \"" + server.URL + "/api\"\n" +
		"[target.api.headers]\nAuthorization = \"Bearer api-token-123\"\n\"Content-Type\" = \"application/vnd.example+json\"\n"
	inv := &invocation{config: config, stdin: strings.NewReader("Subject: disk @channel\n\nsda <failed> & gone\n")}
	if code := inv.run(t); code != 0 {
		t.Fatalf("Run() = %d, want 0; log:\n%s", code, inv.log("slendmail"))
	}
	t.Run("mattermost-username-and-channel", func(t *testing.T) {
		body := bodies["/mm"]
		text, _ := body["text"].(string)
		if body["username"] != "slendmail" || body["channel"] != "alerts" || !strings.HasPrefix(text, "#### disk @\u200bchannel") {
			t.Errorf("body = %v", body)
		}
	})
	t.Run("slack-webhook-escapes-markup", func(t *testing.T) {
		text, _ := bodies["/sw"]["text"].(string)
		if !strings.Contains(text, "sda &lt;failed&gt; &amp; gone") || len(bodies["/sw"]) != 1 {
			t.Errorf("body = %v", bodies["/sw"])
		}
	})
	t.Run("generic-json-with-headers", func(t *testing.T) {
		req := requests["/api"]
		if req == nil || req.Header.Get("Authorization") != "Bearer api-token-123" || req.Header.Get("Content-Type") != "application/vnd.example+json" {
			t.Fatalf("request = %v", req)
		}
		if body := bodies["/api"]; body["subject"] != "disk @channel" || body["body"] != "sda <failed> & gone\n" {
			t.Errorf("body = %v", body)
		}
	})
}

// TestRunSlackWebhookLimit pins that the slack-webhook preset is cut to
// the 40000 characters Slack keeps, with the notice, and stays valid JSON.
// The limit counts the JSON document, where a line break takes two units,
// so a text of short lines keeps somewhat less.
func TestRunSlackWebhookLimit(t *testing.T) {
	var body map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("receiver: %v", err)
		}
	}))
	defer server.Close()
	config := "[target.sw]\ntype = \"http\"\npreset = \"slack-webhook\"\nurl = \"" + server.URL + "/sw\"\n"
	inv := &invocation{config: config, stdin: strings.NewReader("Subject: big\n\n" + strings.Repeat("log line\n", 6000))}
	if code := inv.run(t); code != 0 {
		t.Fatalf("Run() = %d, want 0", code)
	}
	if n := len([]rune(body["text"])); n > 40000 || n < 30000 || !strings.HasSuffix(body["text"], "[truncated, 52.8 KiB in full]```") {
		t.Errorf("text of %d characters, ends %q", n, body["text"][max(0, len(body["text"])-30):])
	}
	if !strings.Contains(inv.log("slendmail"), `msg="text truncated for target" target=sw`) {
		t.Errorf("log lacks the truncation record:\n%s", inv.log("slendmail"))
	}
}

// TestRunLongTextAsMessage pins the keys of a long text from the file to
// the service: max_lines cuts the body, and long_file = "eml" sends the
// message itself, without its Bcc field, as message.eml.
func TestRunLongTextAsMessage(t *testing.T) {
	var mu sync.Mutex
	var published, file string
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		data, _ := io.ReadAll(r.Body)
		if r.Method == http.MethodPut {
			file = r.URL.Query().Get("filename") + "\n" + string(data)
			return
		}
		published = string(data)
	}))
	defer server.Close()
	config := "[target.phone]\ntype = \"ntfy\"\nurl = \"" + server.URL + "/alerts\"\nlong_file = \"eml\"\nmax_lines = 1\n"
	input := "Subject: s\nTo: a@example.org\nBcc: hidden@example.org\n\nfirst line\nsecond line\n"
	inv := &invocation{config: config, args: []string{"-t"}, stdin: strings.NewReader(input)}
	if code := inv.run(t); code != 0 {
		t.Fatalf("Run() = %d, want 0", code)
	}
	if !strings.Contains(published, `first line\n[truncated]\nmessage.eml (message/rfc822`) || strings.Contains(published, "second line") {
		t.Errorf("published %s", published)
	}
	if want := "message.eml\nSubject: s\nTo: a@example.org\n\nfirst line\nsecond line\n"; file != want {
		t.Errorf("file %q, want %q", file, want)
	}
}

// TestBuildTargetsCarriesLimits pins that the limits of a target in the
// configuration reach delivery, where they replace those of the service.
func TestBuildTargetsCarriesLimits(t *testing.T) {
	cfg := &config.Config{Targets: map[string]config.Target{"phone": {
		Type: config.TypeNtfy, URL: "https://ntfy.example.org/alerts", MaxText: 1000, MaxLines: 40, MaxFileSize: 2000000,
	}}}
	targets, err := buildTargets(cfg, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	if got := targets[0]; got.MaxText != 1000 || got.MaxLines != 40 || got.MaxFileSize != 2000000 {
		t.Errorf("target limits %d, %d, %d; want 1000, 40, 2000000", got.MaxText, got.MaxLines, got.MaxFileSize)
	}
}

// TestLogTextRejected pins the warning for a target that rejected the
// text: sent as file only when the file went through, so that a failed
// retry does not claim a delivery.
func TestLogTextRejected(t *testing.T) {
	rejected := &backend.Error{Class: backend.Permanent, Status: 400, Err: errors.New("can't parse entities"), IsTextRejected: true}
	for _, tc := range []struct {
		name   string
		result delivery.Result
		want   string
	}{
		{"file-sent", delivery.Result{TargetID: "tg", Status: delivery.OK, TextRejected: rejected}, `level=WARN msg="text rejected, sent as file" target=tg err="permanent failure, status 400: can't parse entities"`},
		{"file-failed", delivery.Result{TargetID: "tg", Status: delivery.Perm, Err: rejected, TextRejected: rejected}, `level=WARN msg="text rejected, file failed too" target=tg err="permanent failure, status 400: can't parse entities"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var b strings.Builder
			logResult(slog.New(slog.NewTextHandler(&b, nil)), tc.result)
			got := b.String()
			if !strings.Contains(got, tc.want) {
				t.Errorf("log %q, want %q", got, tc.want)
			}
			if tc.result.Status != delivery.OK && strings.Contains(got, "sent as file") {
				t.Errorf("log of a failed retry claims the file: %q", got)
			}
		})
	}
}

// TestRegisterHeaderSecrets pins which header values the redactor masks:
// the whole value and the credential after a scheme, but not a value too
// short to be a credential, which would mask parts of every record.
func TestRegisterHeaderSecrets(t *testing.T) {
	cfg := &config.Config{Targets: map[string]config.Target{"api": {Type: config.TypeHTTP, Headers: map[string]string{
		"Authorization": "Bearer api-token-123", "X-Api-Key": "k3y-v4lue-long", "X-Retry": "1",
	}}}}
	redactor := &redact.Redactor{}
	registerSecrets(redactor, cfg)
	got := redactor.String("auth Bearer api-token-123, token api-token-123, key k3y-v4lue-long, status 401")
	if want := "auth ***, token ***, key ***, status 401"; got != want {
		t.Errorf("redacted = %q, want %q", got, want)
	}
}
