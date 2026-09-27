package app

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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
