package app

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"

	"github.com/6RUN0/mailcrier/internal/delivery"
)

// statusReceiver answers every request with one status after a delay and
// keeps count of the requests and their bodies.
type statusReceiver struct {
	*httptest.Server
	requests atomic.Int64
	bodies   chan string
}

// newStatusServer starts a receiver that answers status, after delay or
// when the client gives up, whichever comes first.
func newStatusServer(t *testing.T, status int, delay time.Duration) *statusReceiver {
	t.Helper()
	s := &statusReceiver{bodies: make(chan string, 16)}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.requests.Add(1)
		body, _ := io.ReadAll(r.Body)
		select {
		case s.bodies <- string(body):
		default:
		}
		if delay > 0 {
			timer := time.NewTimer(delay)
			defer timer.Stop()
			select {
			case <-r.Context().Done():
				return
			case <-timer.C:
			}
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(s.Close)
	return s
}

// httpTarget is an http target name that posts to url.
func httpTarget(name, url string) string {
	return "[target." + name + "]\ntype = \"http\"\npreset = \"generic-json\"\nurl = \"" + url + "\"\n"
}

// probe runs --probe with args after it as an unelevated caller.
func probe(t *testing.T, doc string, args ...string) (int, *invocation) {
	t.Helper()
	inv := &invocation{config: doc, args: append([]string{"--probe"}, args...), creds: plainUser, stdin: iotest.ErrReader(errors.New("stdin read"))}
	return inv.run(t), inv
}

// probeCase runs --probe on the targets a and b, each on a receiver of
// its own status, and checks the exit status.
type probeCase struct {
	statusA, statusB int
	want             int
}

func (tc probeCase) check(t *testing.T) {
	t.Helper()
	a, b := newStatusServer(t, tc.statusA, 0), newStatusServer(t, tc.statusB, 0)
	code, inv := probe(t, httpTarget("a", a.URL)+httpTarget("b", b.URL))
	if code != tc.want {
		t.Fatalf("Run() = %d, want %d; output:\n%s", code, tc.want, inv.output())
	}
	if a.requests.Load() != 1 || b.requests.Load() != 1 {
		t.Errorf("requests a=%d b=%d, want one each", a.requests.Load(), b.requests.Load())
	}
}

func TestProbe(t *testing.T) {
	t.Run("T-ADJ-31/all-ok", probeCase{200, 200, 0}.check)
	t.Run("T-ADJ-31/perm", probeCase{400, 400, 69}.check)
	t.Run("T-ADJ-31/temp", probeCase{503, 503, 75}.check)
	t.Run("ok-and-temp", probeCase{200, 503, 75}.check)
	t.Run("ok-and-perm", probeCase{200, 400, 69}.check)
	t.Run("temp-and-perm", probeCase{503, 400, 69}.check)
	t.Run("sample-message", func(t *testing.T) {
		server := newStatusServer(t, 200, 0)
		code, inv := probe(t, httpTarget("a", server.URL))
		if code != 0 {
			t.Fatalf("Run() = %d, want 0; output:\n%s", code, inv.output())
		}
		if body := <-server.bodies; !strings.Contains(body, "mailcrier probe from host1.example.org") {
			t.Errorf("receiver got %q", body)
		}
		if got := inv.stdout.String(); got != "target=a class=ok\n" {
			t.Errorf("stdout = %q", got)
		}
		if log := inv.log("mailcrier"); !strings.Contains(log, `level=INFO msg="probe sent" mode=probe count=1`) {
			t.Errorf("log lacks the record:\n%s", log)
		}
	})
	t.Run("rules-do-not-apply", func(t *testing.T) {
		server := newStatusServer(t, 200, 0)
		doc := httpTarget("a", server.URL) + httpTarget("b", server.URL) + "[[suppress]]\nsubject = \"mailcrier probe*\"\n[[route]]\ntargets = [\"a\"]\n"
		if code, inv := probe(t, doc); code != 0 || server.requests.Load() != 2 {
			t.Errorf("Run() = %d with %d requests, want 0 and 2; output:\n%s", code, server.requests.Load(), inv.output())
		}
	})
	t.Run("named-target", func(t *testing.T) {
		a, b := newStatusServer(t, 200, 0), newStatusServer(t, 200, 0)
		code, inv := probe(t, httpTarget("a", a.URL)+httpTarget("b", b.URL), "--", "a", "a")
		if code != 0 || a.requests.Load() != 1 || b.requests.Load() != 0 {
			t.Errorf("Run() = %d, requests a=%d b=%d, want 0, 1 and 0; output:\n%s", code, a.requests.Load(), b.requests.Load(), inv.output())
		}
	})
	t.Run("unknown-target", func(t *testing.T) {
		a := newStatusServer(t, 200, 0)
		code, inv := probe(t, httpTarget("a", a.URL), "--", "a", "nope")
		if code != 64 || a.requests.Load() != 0 {
			t.Errorf("Run() = %d with %d requests, want 64 and none", code, a.requests.Load())
		}
		if got := inv.stderr.String(); got != "mailcrier: --probe: no target \"nope\"\n" {
			t.Errorf("stderr = %q", got)
		}
	})
	t.Run("network-unreachable", func(t *testing.T) {
		if code, inv := probe(t, httpTarget("a", "http://127.0.0.1:1")); code != 75 {
			t.Errorf("Run() = %d, want 75; output:\n%s", code, inv.output())
		}
	})
	t.Run("request-timeout", func(t *testing.T) {
		server := newStatusServer(t, 200, time.Minute)
		if code, inv := probe(t, "[general]\nhttp_timeout = \"50ms\"\n\n"+httpTarget("a", server.URL)); code != 75 {
			t.Errorf("Run() = %d, want 75; output:\n%s", code, inv.output())
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		// The server notices that the client left only once the body is
		// read.
		server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			cancel()
			<-r.Context().Done()
		}))
		defer server.Close()
		inv := &invocation{config: httpTarget("a", server.URL), args: []string{"--probe"}, ctx: ctx, stdin: iotest.ErrReader(errors.New("stdin read"))}
		if code := inv.run(t); code != 75 {
			t.Errorf("Run() = %d, want 75; output:\n%s", code, inv.output())
		}
	})
	t.Run("template-falls-back", func(t *testing.T) {
		server := newStatusServer(t, 200, 0)
		doc := "[target.a]\ntype = \"http\"\nurl = \"" + server.URL + "\"\ntemplate = \"{{ .Nope }}\"\n" + httpTarget("b", server.URL)
		code, inv := probe(t, doc)
		if code != 0 {
			t.Fatalf("Run() = %d, want 0; output:\n%s", code, inv.output())
		}
		if got, want := inv.stdout.String(), "target=a class=ok template=fallback\ntarget=b class=ok\n"; got != want {
			t.Errorf("stdout = %q, want %q", got, want)
		}
	})
	t.Run("status-line", func(t *testing.T) {
		server := newStatusServer(t, 503, 0)
		code, inv := probe(t, httpTarget("a", server.URL))
		if code != 75 {
			t.Fatalf("Run() = %d, want 75", code)
		}
		if got := inv.stdout.String(); !strings.HasPrefix(got, "target=a class=temp status=503 err=\"") {
			t.Errorf("stdout = %q", got)
		}
	})
	t.Run("token-masked", func(t *testing.T) {
		// Slack quotes the token of a request it refuses.
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `{"ok":false,"error":"invalid_auth for `+r.Header.Get("Authorization")+`"}`)
		}))
		defer server.Close()
		inv := &invocation{config: slackConfig(secretToken), client: rewritingClient(server), args: []string{"--probe"}, stdin: iotest.ErrReader(errors.New("stdin read"))}
		if code := inv.run(t); code != 69 {
			t.Fatalf("Run() = %d, want 69; output:\n%s", code, inv.output())
		}
		stdout := inv.stdout.String()
		if !strings.Contains(stdout, "***") || strings.Contains(stdout+inv.output(), secretToken) {
			t.Errorf("token not masked:\n%s\n%s", stdout, inv.output())
		}
	})
	t.Run("telegram-token-masked", func(t *testing.T) {
		// The fake Bot API quotes the path, which holds the token.
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"ok":false,"error_code":400,"description":"Bad Request: no route for `+r.URL.Path+`"}`)
		}))
		defer server.Close()
		inv := &invocation{
			config: "[target.tg]\ntype = \"telegram\"\ntoken = \"" + secretToken + "\"\nchat_id = \"1\"\n", client: rewritingClient(server),
			args: []string{"--probe"}, stdin: iotest.ErrReader(errors.New("stdin read")),
		}
		if code := inv.run(t); code != 69 {
			t.Fatalf("Run() = %d, want 69; output:\n%s", code, inv.output())
		}
		stdout := inv.stdout.String()
		if !strings.HasPrefix(stdout, "target=tg class=perm status=400 ") || !strings.Contains(stdout, "/bot***/") || strings.Contains(stdout+inv.output(), secretToken) {
			t.Errorf("token not masked:\n%s\n%s", stdout, inv.output())
		}
	})
	t.Run("configuration-rejected", func(t *testing.T) {
		server := newStatusServer(t, 200, 0)
		code, inv := probe(t, httpTarget("a", server.URL)+"bogus = 1\n")
		if code != 78 || server.requests.Load() != 0 {
			t.Errorf("Run() = %d with %d requests, want 78 and none", code, server.requests.Load())
		}
		if got := inv.stderr.String(); !strings.HasPrefix(got, "mailcrier: /etc/mailcrier.conf:5:1: unknown key") {
			t.Errorf("stderr = %q", got)
		}
	})
	t.Run("configuration-missing", func(t *testing.T) {
		code, inv := probe(t, "", "--config", "/etc/missing.conf")
		if code != 78 {
			t.Errorf("Run() = %d, want 78", code)
		}
		if got := inv.stderr.String(); got != "mailcrier: /etc/missing.conf: file does not exist\n" {
			t.Errorf("stderr = %q", got)
		}
	})
}

// TestProbeRunsHooks pins that an exec target gets the sample message on
// stdin and decides the exit status like any target.
func TestProbeRunsHooks(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status string
		want   int
	}{{"delivered", "0", 0}, {"rejected", "1", 69}, {"temporary", "75", 75}} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeHookScript(t, "cat > \"$0.stdin\"\nexit "+tc.status+"\n")
			code, inv := probe(t, "[target.run]\ntype = \"exec\"\nargv = [\""+path+"\"]\n")
			if code != tc.want {
				t.Fatalf("Run() = %d, want %d; output:\n%s", code, tc.want, inv.output())
			}
			stdin, err := os.ReadFile(path + ".stdin")
			if err != nil || !strings.Contains(string(stdin), "Subject: mailcrier probe from host1.example.org\n") {
				t.Errorf("hook stdin = %q, %v", stdin, err)
			}
		})
	}
}

// TestProbeLeavesSpool pins that --probe neither runs the queue nor
// releases held messages, nor writes anything to the spool: the entries a
// call left are byte for byte the same afterwards.
func TestProbeLeavesSpool(t *testing.T) {
	c := newSpoolCase(t)
	c.service.reply("a", delivery.Temp)
	if code, inv := c.send("queued", plainUser); code != 0 {
		t.Fatalf("send = %d; output:\n%s", code, inv.output())
	}
	c.config = "bogus = 1\n"
	if code, inv := c.send("held", plainUser); code != 78 {
		t.Fatalf("send = %d; output:\n%s", code, inv.output())
	}
	c.clock.advance(time.Hour)
	before := spoolFiles(t, c.dir)
	if len(c.ids("queue")) != 1 || len(c.ids("hold")) != 1 {
		t.Fatalf("spool holds %v", before)
	}
	server := newStatusServer(t, 200, 0)
	inv := &invocation{config: httpTarget("a", server.URL) + httpTarget("b", server.URL), args: []string{"--probe"}, creds: plainUser, spoolDir: c.dir, now: c.clock.now,
		stdin: iotest.ErrReader(errors.New("stdin read"))}
	if code := inv.run(t); code != 0 {
		t.Fatalf("Run() = %d, want 0; output:\n%s", code, inv.output())
	}
	if server.requests.Load() != 2 {
		t.Errorf("receiver got %d requests, want 2", server.requests.Load())
	}
	if after := spoolFiles(t, c.dir); !maps.Equal(before, after) {
		t.Errorf("spool changed:\nbefore %v\nafter  %v", before, after)
	}
}

// spoolFiles maps the files under dir to their content.
func spoolFiles(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		files[strings.TrimPrefix(path, dir)] = string(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}
