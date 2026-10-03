package app

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// bodyServer records the body and the Content-Type of every request by
// path.
type bodyServer struct {
	mu           sync.Mutex
	bodies       map[string]string
	contentTypes map[string]string
}

func newBodyServer(t *testing.T) (*bodyServer, *httptest.Server) {
	t.Helper()
	s := &bodyServer{bodies: map[string]string{}, contentTypes: map[string]string{}}
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		s.bodies[r.URL.Path], s.contentTypes[r.URL.Path] = string(body), r.Header.Get("Content-Type")
	}))
	t.Cleanup(server.Close)
	return s, server
}

// parseErrorCase is a template that does not parse and a text the log
// must hold.
type parseErrorCase struct {
	template, want string
}

func (c parseErrorCase) check(t *testing.T) {
	dir := t.TempDir()
	config := "[target.api]\ntype = \"http\"\nurl = \"http://127.0.0.1:1\"\ntemplate = '" + c.template + "'\n"
	inv := &invocation{config: config, spoolDir: dir, stdin: strings.NewReader("Subject: t\n\nb\n")}
	if code := inv.run(t); code != 78 {
		t.Fatalf("Run() = %d, want 78; log:\n%s", code, inv.log("mailcrier"))
	}
	log := inv.log("mailcrier")
	if !strings.Contains(log, "configuration rejected") || !strings.Contains(log, c.want) {
		t.Errorf("log lacks %q:\n%s", c.want, log)
	}
	held, err := filepath.Glob(filepath.Join(dir, "hold", "*.eml"))
	if err != nil || len(held) != 1 {
		t.Errorf("held %v, %v", held, err)
	}
}

// TestRunTemplateParseError pins that a template that does not parse
// rejects the configuration: exit 78, the message held, and the log names
// the target and the position in the template.
func TestRunTemplateParseError(t *testing.T) {
	t.Run("T-TPL-04/syntax-error", parseErrorCase{`{"s": {{ toJson .Subject }`, `target \"api\": template: api:1:`}.check)
	t.Run("T-TPL-04/unknown-function", parseErrorCase{`{{ now }}`, `function \"now\" not defined`}.check)
	t.Run("T-TPL-04/define-rejected", parseErrorCase{`{{ define "x" }}{{ end }}{}`, `{{define}} is not allowed`}.check)
	t.Run("T-TPL-04/queue-run", func(t *testing.T) {
		config := "[target.api]\ntype = \"http\"\nurl = \"http://127.0.0.1:1\"\ntemplate = '{{ now }}'\n"
		inv := &invocation{config: config, spoolDir: t.TempDir(), args: []string{"-q"}, stdin: strings.NewReader("")}
		if code := inv.run(t); code != 78 {
			t.Fatalf("Run(-q) = %d, want 78; log:\n%s", code, inv.log("mailcrier"))
		}
		if log := inv.log("mailcrier"); !strings.Contains(log, "configuration rejected") || !strings.Contains(log, `target \"api\": template: api:1:`) {
			t.Errorf("log:\n%s", log)
		}
	})
}

// TestRunUserTemplates runs whole invocations with templates from the
// configuration: an http target without preset sends the rendered JSON,
// and a failed template falls back to the built-in one with a warning,
// the call exiting 0.
func TestRunUserTemplates(t *testing.T) {
	received, server := newBodyServer(t)
	config := "[target.api]\ntype = \"http\"\nurl = \"" + server.URL + "/api\"\n" +
		"template = '{\"title\": {{ .Subject | toUpper | toJson }}, \"lines\": {{ lines .Body | len }}}'\n\n" +
		"[target.broken]\ntype = \"http\"\nurl = \"" + server.URL + "/broken\"\ntemplate = '{{ .Date | tz \"Mars/Olympus\" }}'\n"
	inv := &invocation{config: config, stdin: strings.NewReader("Subject: disk failed\n\none\ntwo\n")}
	if code := inv.run(t); code != 0 {
		t.Fatalf("Run() = %d, want 0; log:\n%s", code, inv.log("mailcrier"))
	}
	t.Run("http-without-preset", func(t *testing.T) {
		if got, want := received.bodies["/api"], `{"title": "DISK FAILED", "lines": 2}`; got != want || received.contentTypes["/api"] != "application/json" {
			t.Errorf("body %q (%s), want %q", got, received.contentTypes["/api"], want)
		}
	})
	t.Run("T-TPL-05/fallback-with-warning", func(t *testing.T) {
		var body map[string]any
		if err := json.Unmarshal([]byte(received.bodies["/broken"]), &body); err != nil || body["subject"] != "disk failed" {
			t.Errorf("body %q, %v, want the generic-json document", received.bodies["/broken"], err)
		}
		log := inv.log("mailcrier")
		want := `level=WARN msg="template failed, built-in used" target=broken err="user template: template: broken:1:`
		if !strings.Contains(log, want) || !strings.Contains(log, "unknown time zone") {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
		if strings.Count(log, "template failed") != 1 {
			t.Errorf("log:\n%s", log)
		}
	})
}

// requestServer records every request: method, URL, headers and body.
type requestServer struct {
	mu       sync.Mutex
	requests []*http.Request
	bodies   []string
}

func newRequestServer(t *testing.T) (*requestServer, *httptest.Server) {
	t.Helper()
	s := &requestServer{}
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		s.requests, s.bodies = append(s.requests, r), append(s.bodies, string(body))
	}))
	t.Cleanup(server.Close)
	return s, server
}

// TestRunRequestParts runs whole invocations of http targets with
// templates in the path, the query and the headers.
func TestRunRequestParts(t *testing.T) {
	stdin := "Subject: disk a/b failed\n\nbody\n"
	t.Run("T-TPL-19/get-with-parts", func(t *testing.T) {
		received, server := newRequestServer(t)
		config := "[target.api]\ntype = \"http\"\nmethod = \"GET\"\nurl = \"" + server.URL + "/in?token=abc\"\n" +
			"path = \"/hosts/{{ pathSegment .Hostname }}/{{ pathSegment .Subject }}\"\n" +
			"[target.api.query]\nsubject = \"{{ .Subject }}\"\n[target.api.headers]\nX-Host = \"{{ .Hostname | toUpper }}\"\n"
		inv := &invocation{config: config, stdin: strings.NewReader(stdin)}
		if code := inv.run(t); code != 0 || len(received.requests) != 1 {
			t.Fatalf("Run() = %d, %d requests; log:\n%s", code, len(received.requests), inv.log("mailcrier"))
		}
		req := received.requests[0]
		if req.Method != http.MethodGet || req.URL.EscapedPath() != "/in/hosts/host1.example.org/disk%20a%2Fb%20failed" ||
			req.URL.RawQuery != "token=abc&subject=disk+a%2Fb+failed" || req.Header.Get("X-Host") != "HOST1.EXAMPLE.ORG" ||
			req.Header.Get("Content-Type") != "" || received.bodies[0] != "" {
			t.Errorf("request %s %s, headers %v, body %q", req.Method, req.URL, req.Header, received.bodies[0])
		}
	})
	t.Run("T-TPL-16/path-to-other-host", rejectedRequestCase{"path", `{{ "//evil.example/" }}`, "path: must start with a single /"}.check)
	t.Run("T-TPL-16/fixed-path-to-other-host", func(t *testing.T) {
		config := "[target.api]\ntype = \"http\"\nmethod = \"GET\"\nurl = \"http://127.0.0.1:1\"\npath = \"//evil.example/{{ .Hostname }}\"\n"
		inv := &invocation{config: config, spoolDir: t.TempDir(), stdin: strings.NewReader(stdin)}
		if code := inv.run(t); code != 78 || !strings.Contains(inv.log("mailcrier"), `value of key \"path\" must start with a single /`) {
			t.Errorf("Run() = %d; log:\n%s", code, inv.log("mailcrier"))
		}
	})
	t.Run("T-TPL-16/path-renders-host", rejectedRequestCase{"path", "{{ .Subject }}", "path: must start with a single /"}.check)
	t.Run("T-TPL-20/header-fails", rejectedRequestCase{"headers", "{{ index .To 5 }}", `level=WARN msg="request template failed, message not sent" target=api`}.check)
	t.Run("T-TPL-18/header-renders-line-break", rejectedRequestCase{"headers", "{{ .Body }}", `header \"X-H\" is invalid`}.check)
	// A permanent failure finishes the entry for the target like any
	// other: it is removed, not moved to failed/, and -q has nothing to
	// retry.
	t.Run("T-TPL-20/spool-entry-finished", func(t *testing.T) {
		received, server := newRequestServer(t)
		dir := t.TempDir()
		config := "[target.api]\ntype = \"http\"\nmethod = \"GET\"\nurl = \"" + server.URL + "/in\"\n[target.api.headers]\nX-H = '{{ index .To 5 }}'\n"
		inv := &invocation{config: config, spoolDir: dir, stdin: strings.NewReader(stdin)}
		if code := inv.run(t); code != 69 {
			t.Fatalf("Run() = %d, want 69; log:\n%s", code, inv.log("mailcrier"))
		}
		queueRun := &invocation{config: config, spoolDir: dir, args: []string{"-q"}, stdin: strings.NewReader("")}
		if code := queueRun.run(t); code != 0 {
			t.Fatalf("Run(-q) = %d; log:\n%s", code, queueRun.log("mailcrier"))
		}
		entries, _ := filepath.Glob(filepath.Join(dir, "*", "*.eml"))
		if len(entries) != 0 || len(received.requests) != 0 || !strings.Contains(inv.log("mailcrier"), "request template failed") {
			t.Errorf("entries %v, %d requests; log:\n%s", entries, len(received.requests), inv.log("mailcrier"))
		}
	})
	t.Run("T-TPL-04/header-parse-error", func(t *testing.T) {
		config := "[target.api]\ntype = \"http\"\nmethod = \"GET\"\nurl = \"http://127.0.0.1:1\"\n[target.api.headers]\nX-H = \"{{ .Subject \"\n"
		inv := &invocation{config: config, spoolDir: t.TempDir(), stdin: strings.NewReader(stdin)}
		if code := inv.run(t); code != 78 || !strings.Contains(inv.log("mailcrier"), `template: api.headers.X-H:1:`) {
			t.Errorf("Run() = %d; log:\n%s", code, inv.log("mailcrier"))
		}
	})
}

// rejectedRequestCase is a request part whose template, or what it
// renders for the message, fails the target before any request.
type rejectedRequestCase struct {
	key, source, want string
}

func (c rejectedRequestCase) check(t *testing.T) {
	received, server := newRequestServer(t)
	config := "[target.api]\ntype = \"http\"\nmethod = \"GET\"\nurl = \"" + server.URL + "/in\"\n"
	if c.key == "path" {
		config += "path = '" + c.source + "'\n"
	} else {
		config += "[target.api.headers]\nX-H = '" + c.source + "'\n"
	}
	inv := &invocation{config: config, stdin: strings.NewReader("Subject: http://evil.example/x\n\nline one\nline two\n")}
	if code := inv.run(t); code != 69 || len(received.requests) != 0 {
		t.Fatalf("Run() = %d, %d requests, want 69 and none; log:\n%s", code, len(received.requests), inv.log("mailcrier"))
	}
	if log := inv.log("mailcrier"); !strings.Contains(log, c.want) || strings.Contains(log, "evil") {
		t.Errorf("log lacks %q or quotes the message:\n%s", c.want, log)
	}
}
