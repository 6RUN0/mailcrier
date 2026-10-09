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

// TestRunKeepsTokenOutOfShoutrrrPanic runs a shoutrrr target whose
// transport panics with the URL in the value: the panic is a failure of
// the target, and the token reaches neither syslog nor stderr.
func TestRunKeepsTokenOutOfShoutrrrPanic(t *testing.T) {
	t.Run("T-TPL-12/shoutrrr-panic", func(t *testing.T) {
		config := "[target.bus]\ntype = \"shoutrrr\"\nurl = \"generic+https://hooks.example.org/hook/" + secretToken + "\"\n"
		inv := &invocation{config: config, client: &http.Client{Transport: panickingTransport{}}, stdin: strings.NewReader("Subject: t\n\nb\n")}
		if code := inv.run(t); code != 69 {
			t.Errorf("Run() = %d, want 69", code)
		}
		output := inv.output()
		if !strings.Contains(output, "panic: unexpected request") {
			t.Errorf("output lacks the panic:\n%s", output)
		}
		if strings.Contains(output, secretToken) {
			t.Errorf("output contains the token:\n%s", output)
		}
	})
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
			line: `error: /etc/mailcrier.conf:3:1: target "bus": unknown shoutrrr service "foo"`}},
		{"url-rejected", checkConfigCase{doc: "[target.bus]\ntype = \"shoutrrr\"\nurl = \"telegram://telegram?chats=1\"\n", want: 78,
			line: `error: /etc/mailcrier.conf:3:1: target "bus": URL rejected by shoutrrr service "telegram": `}},
		// The library quotes the token it rejects.
		{"url-rejected-token-masked", checkConfigCase{doc: "[target.bus]\ntype = \"shoutrrr\"\nurl = \"telegram://SECRET@telegram?chats=1\"\n", want: 78,
			line: `error: /etc/mailcrier.conf:3:1: target "bus": URL rejected by shoutrrr service "telegram": telegram: invalid telegram token: ***:`}},
		{"native-type", checkConfigCase{doc: "[target.bus]\ntype = \"shoutrrr\"\nurl = \"telegram://123456:ABC@telegram?chats=1\"\n",
			line: `warning: /etc/mailcrier.conf:3:1: target "bus": shoutrrr service "telegram" has a native target type "telegram"`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, tc.check)
	}
}
