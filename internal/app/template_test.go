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
		t.Fatalf("Run() = %d, want 78; log:\n%s", code, inv.log("slendmail"))
	}
	log := inv.log("slendmail")
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
			t.Fatalf("Run(-q) = %d, want 78; log:\n%s", code, inv.log("slendmail"))
		}
		if log := inv.log("slendmail"); !strings.Contains(log, "configuration rejected") || !strings.Contains(log, `target \"api\": template: api:1:`) {
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
		t.Fatalf("Run() = %d, want 0; log:\n%s", code, inv.log("slendmail"))
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
		log := inv.log("slendmail")
		want := `level=WARN msg="template failed, built-in used" target=broken err="user template: template: broken:1:`
		if !strings.Contains(log, want) || !strings.Contains(log, "unknown time zone") {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
		if strings.Count(log, "template failed") != 1 {
			t.Errorf("log:\n%s", log)
		}
	})
}
