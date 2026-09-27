package app

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
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
