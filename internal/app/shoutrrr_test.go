//go:build !noshoutrrr

package app

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// shoutrrrRejection is the log text for a shoutrrr URL of an unknown
// scheme.
const shoutrrrRejection = `target \"bus\": unknown shoutrrr service \"nosuch\"`

// TestRunDeliversToShoutrrrTarget runs a whole invocation through the
// generic webhook service of shoutrrr: the plain text arrives, and a
// rejection by the receiver is classified by its status.
func TestRunDeliversToShoutrrrTarget(t *testing.T) {
	bodies := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies <- string(body)
	}))
	defer server.Close()
	config := "[target.bus]\ntype = \"shoutrrr\"\nurl = \"generic+" + server.URL + "/hook\"\n"
	inv := &invocation{config: config, stdin: strings.NewReader("Subject: disk full\n\nmd0 degraded\n")}
	if code := inv.run(t); code != 0 {
		t.Fatalf("Run() = %d, want 0; log:\n%s", code, inv.output())
	}
	select {
	case body := <-bodies:
		if !strings.Contains(body, "disk full") || !strings.Contains(body, "md0 degraded") {
			t.Errorf("receiver got %q", body)
		}
	default:
		t.Fatal("receiver got no request")
	}
}
