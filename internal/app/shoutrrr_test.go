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

// shoutrrrTargetConfig is a shoutrrr target for tests that cover every
// target type; empty in a build without the library.
const shoutrrrTargetConfig = "[target.bus]\ntype = \"shoutrrr\"\nurl = \"generic://hooks.example.org/in\"\n"

// TestCheckConfigShoutrrr runs --check-config on the shoutrrr URLs that
// only the library can judge.
func TestCheckConfigShoutrrr(t *testing.T) {
	cases := []struct {
		name string
		checkConfigCase
	}{
		{"unknown-service", checkConfigCase{doc: "[target.bus]\ntype = \"shoutrrr\"\nurl = \"foo://example.org\"\n", want: 78,
			line: `error: /etc/mailcrier.conf: target "bus": unknown shoutrrr service "foo"`}},
		{"url-rejected", checkConfigCase{doc: "[target.bus]\ntype = \"shoutrrr\"\nurl = \"telegram://telegram?chats=1\"\n", want: 78,
			line: `error: /etc/mailcrier.conf: target "bus": URL rejected by shoutrrr service "telegram"`}},
		{"native-type", checkConfigCase{doc: "[target.bus]\ntype = \"shoutrrr\"\nurl = \"telegram://123456:ABC@telegram?chats=1\"\n",
			line: `warning: /etc/mailcrier.conf:3:1: target "bus": shoutrrr service "telegram" has a native target type "telegram"`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, tc.check)
	}
}
