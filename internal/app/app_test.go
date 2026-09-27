package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"testing/iotest"
)

// invocation is one Run call against an in-memory configuration with the
// log captured per syslog tag.
type invocation struct {
	config string
	stdin  io.Reader
	client *http.Client
	logs   map[string]*bytes.Buffer
}

func (inv *invocation) run(t *testing.T) int {
	t.Helper()
	inv.logs = map[string]*bytes.Buffer{}
	client := inv.client
	if client == nil {
		client = &http.Client{}
	}
	deps := Deps{
		NewLogger: func(tag string) *slog.Logger {
			buf := &bytes.Buffer{}
			inv.logs[tag] = buf
			return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
		},
		ConfigFS:   fstest.MapFS{SystemConfigPath: {Data: []byte(inv.config)}},
		ConfigPath: SystemConfigPath,
		HTTP:       client,
		Hostname:   "host1.example.org",
	}
	return Run(context.Background(), deps, []string{"-ti"}, inv.stdin)
}

func (inv *invocation) log(tag string) string {
	if buf, ok := inv.logs[tag]; ok {
		return buf.String()
	}
	return ""
}

func httpTargetConfig(url string) string {
	return "[target.hook]\ntype = \"http\"\npreset = \"generic-json\"\nurl = \"" + url + "\"\n"
}

// TestRunDeliversToHTTPTarget runs a whole invocation against a local
// receiver: the message read from stdin arrives as the generic-json
// document and the exit status is 0.
func TestRunDeliversToHTTPTarget(t *testing.T) {
	type request struct {
		method, path, contentType string
		body                      map[string]string
	}
	requests := make(chan request, 1)
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("receiver: body is not a JSON object: %v", err)
		}
		requests <- request{r.Method, r.URL.Path, r.Header.Get("Content-Type"), body}
	}))
	defer server.Close()

	inv := &invocation{config: httpTargetConfig(server.URL + "/hook"), stdin: strings.NewReader("Subject: t\n\nb\n")}
	if code := inv.run(t); code != 0 {
		t.Fatalf("Run() = %d, want 0; log:\n%s", code, inv.log("slendmail"))
	}
	// Run has returned, so a delivered request is already in the channel.
	var got request
	select {
	case got = <-requests:
	default:
		t.Fatal("receiver got no request")
	}
	want := request{"POST", "/hook", "application/json", map[string]string{"subject": "t", "body": "b\n", "hostname": "host1.example.org"}}
	if got.method != want.method || got.path != want.path || got.contentType != want.contentType {
		t.Errorf("request = %s %s %s, want %s %s %s", got.method, got.path, got.contentType, want.method, want.path, want.contentType)
	}
	for key, value := range want.body {
		if got.body[key] != value {
			t.Errorf("body[%q] = %q, want %q", key, got.body[key], value)
		}
	}
	if len(got.body) != len(want.body) {
		t.Errorf("body = %v, want exactly %v", got.body, want.body)
	}
	if log := inv.log("slendmail"); !strings.Contains(log, "target delivered") || !strings.Contains(log, "target=hook") {
		t.Errorf("log does not record the delivery:\n%s", log)
	}
}

func TestRunUsesConfiguredSyslogTag(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()

	inv := &invocation{
		config: "[general]\nsyslog_tag = \"mailbot\"\n\n" + httpTargetConfig(server.URL),
		stdin:  strings.NewReader("Subject: t\n\nb\n"),
	}
	if code := inv.run(t); code != 0 {
		t.Fatalf("Run() = %d, want 0", code)
	}
	if !strings.Contains(inv.log("mailbot"), "target delivered") {
		t.Errorf("delivery not logged under the configured tag; logs: %v", inv.logs)
	}
}

func TestRunRejectsConfiguration(t *testing.T) {
	cases := []struct {
		name    string
		config  string
		wantLog string
	}{
		{"unknown-key", httpTargetConfig("http://127.0.0.1:1") + "colour = \"red\"\n", `unknown key \"target.hook.colour\"`},
		{"unparsable", "[target.hook\n", "expected"},
		{"no-targets", "", "no targets configured"},
		{"type-not-implemented", "[target.tg]\ntype = \"telegram\"\ntoken = \"1:a\"\n", `type \"telegram\" is not implemented`},
		{"preset-not-implemented", "[target.mm]\ntype = \"http\"\nurl = \"http://127.0.0.1:1\"\npreset = \"mattermost\"\n", `preset \"mattermost\" is not implemented`},
	}
	for _, tc := range cases {
		t.Run("T-ADJ-28/"+tc.name, func(t *testing.T) {
			inv := &invocation{config: tc.config, stdin: strings.NewReader("Subject: t\n\nb\n")}
			if code := inv.run(t); code != 78 {
				t.Fatalf("Run() = %d, want 78", code)
			}
			log := inv.log("slendmail")
			if !strings.Contains(log, "level=ERROR") || !strings.Contains(log, "configuration rejected") || !strings.Contains(log, tc.wantLog) {
				t.Errorf("log lacks the error record with %q:\n%s", tc.wantLog, log)
			}
		})
	}
}

func TestRunReportsUndeliveredMessage(t *testing.T) {
	cases := []struct {
		name   string
		status int
	}{
		{"rejected-by-receiver", http.StatusBadRequest},
		{"receiver-error-without-spool", http.StatusBadGateway},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
			}))
			defer server.Close()

			inv := &invocation{config: httpTargetConfig(server.URL), stdin: strings.NewReader("Subject: t\n\nb\n")}
			if code := inv.run(t); code != 69 {
				t.Fatalf("Run() = %d, want 69", code)
			}
			if log := inv.log("slendmail"); !strings.Contains(log, "target failed") {
				t.Errorf("log lacks the failure:\n%s", log)
			}
		})
	}
}

func TestRunReportsUnreadableInput(t *testing.T) {
	inv := &invocation{config: httpTargetConfig("http://127.0.0.1:1"), stdin: iotest.ErrReader(errors.New("input/output error"))}
	if code := inv.run(t); code != 66 {
		t.Fatalf("Run() = %d, want 66", code)
	}
}
