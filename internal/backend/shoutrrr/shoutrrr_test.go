//go:build !noshoutrrr

package shoutrrr

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/6RUN0/mailcrier/internal/backend"
)

// genericURL returns the URL of the generic webhook service of shoutrrr
// for a plain HTTP receiver.
func genericURL(server *httptest.Server, path string) string {
	return "generic+" + server.URL + path
}

func TestSendPostsText(t *testing.T) {
	type request struct{ method, path, body string }
	requests := make(chan request, 1)
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requests <- request{r.Method, r.URL.Path, string(body)}
	}))
	defer server.Close()
	sender, err := New(Options{URL: genericURL(server, "/hook"), Client: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if err := sender.Send(context.Background(), backend.Payload{Title: "s", Text: "line one\nline two"}); err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	if got := <-requests; got.method != http.MethodPost || got.path != "/hook" || got.body != "line one\nline two" {
		t.Errorf("receiver got %+v", got)
	}
}

// TestSendClassifiesStatus pins that the status of the answer decides the
// class, as for the native targets, though shoutrrr reports it as text.
func TestSendClassifiesStatus(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		header     string
		want       backend.Class
		retryAfter time.Duration
	}{
		{"server-error", http.StatusBadGateway, "", backend.Temporary, 0},
		{"rate-limited", http.StatusTooManyRequests, "30", backend.Temporary, 30 * time.Second},
		{"rejected", http.StatusBadRequest, "", backend.Permanent, 0},
		{"redirect", http.StatusFound, "", backend.Permanent, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if tc.header != "" {
					w.Header().Set("Retry-After", tc.header)
				}
				if tc.status == http.StatusFound {
					http.Redirect(w, r, "/elsewhere", tc.status)
					return
				}
				w.WriteHeader(tc.status)
			}))
			defer server.Close()
			sender, err := New(Options{URL: genericURL(server, "/hook"), Client: server.Client()})
			if err != nil {
				t.Fatal(err)
			}
			err = sender.Send(context.Background(), backend.Payload{Text: "t"})
			var deliveryErr *backend.Error
			if !errors.As(err, &deliveryErr) {
				t.Fatalf("Send() error = %v, want *backend.Error", err)
			}
			if deliveryErr.Class != tc.want || deliveryErr.Status != tc.status || deliveryErr.RetryAfter != tc.retryAfter {
				t.Errorf("error = %+v, want class %v, status %d, retry after %v", deliveryErr, tc.want, tc.status, tc.retryAfter)
			}
			if requests != 1 {
				t.Errorf("receiver got %d requests, want 1: a redirect is not followed", requests)
			}
		})
	}
}

func TestSendTransportErrorIsTemporary(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	endpoint := genericURL(server, "/hook/SECRET-TOKEN")
	server.Close()
	sender, err := New(Options{URL: endpoint, Client: &http.Client{}})
	if err != nil {
		t.Fatal(err)
	}
	err = sender.Send(context.Background(), backend.Payload{Text: "t"})
	var deliveryErr *backend.Error
	if !errors.As(err, &deliveryErr) || deliveryErr.Class != backend.Temporary || deliveryErr.Status != 0 {
		t.Fatalf("Send() error = %v, want temporary without status", err)
	}
	if strings.Contains(err.Error(), "SECRET-TOKEN") {
		t.Errorf("error text contains the URL: %v", err)
	}
}

// TestSendStopsAtDeadline pins that the deadline of the call ends a send
// that hangs, as a temporary failure.
func TestSendStopsAtDeadline(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer server.Close()
	defer close(release)
	sender, err := New(Options{URL: genericURL(server, "/hook"), Client: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err = sender.Send(ctx, backend.Payload{Text: "t"})
	var deliveryErr *backend.Error
	if !errors.As(err, &deliveryErr) || deliveryErr.Class != backend.Temporary {
		t.Errorf("Send() error = %v, want temporary", err)
	}
}

// TestNewRejectsURL pins that a URL shoutrrr cannot use fails with the
// configuration, naming the scheme but not the URL.
func TestNewRejectsURL(t *testing.T) {
	for url, want := range map[string]string{
		"nosuch://SECRET-TOKEN@example.org":     `unknown shoutrrr service "nosuch"`,
		"telegram://SECRET-TOKEN@telegram?x=%%": `URL rejected by shoutrrr service "telegram"`,
	} {
		_, err := New(Options{URL: url, Client: &http.Client{}})
		if err == nil || err.Error() != want {
			t.Errorf("New(%s) error = %v, want %s", url, err, want)
		}
	}
}

// TestSendWaitsForAbandonedSend pins that a send given up at the deadline
// keeps the target busy until the library returns: the next send does not
// run beside it, where the late 302 of the abandoned request would land in
// the recorder and decide the class of the other send.
func TestSendWaitsForAbandonedSend(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			<-release
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
		}
	}))
	defer server.Close()
	defer releaseOnce.Do(func() { close(release) })
	sender, err := New(Options{URL: genericURL(server, "/hook"), Client: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	short := func() context.Context {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		t.Cleanup(cancel)
		return ctx
	}

	var deliveryErr *backend.Error
	if err := sender.Send(short(), backend.Payload{Text: "first"}); !errors.As(err, &deliveryErr) || deliveryErr.Class != backend.Temporary {
		t.Fatalf("first Send() error = %v, want temporary at the deadline", err)
	}
	err = sender.Send(short(), backend.Payload{Text: "second"})
	if !errors.As(err, &deliveryErr) || deliveryErr.Class != backend.Temporary || !strings.Contains(err.Error(), "still running") {
		t.Fatalf("second Send() error = %v, want temporary, previous send still running", err)
	}
	if n := requests.Load(); n != 1 {
		t.Fatalf("receiver got %d requests while the first hung, want 1", n)
	}
	releaseOnce.Do(func() { close(release) })
	if err := sender.Send(context.Background(), backend.Payload{Text: "third"}); err != nil {
		t.Errorf("third Send() error = %v, want delivered: the receiver answered 200", err)
	}
	if n := requests.Load(); n != 2 {
		t.Errorf("receiver got %d requests, want 2", n)
	}
}

// TestNewRejectsMatrixLogin pins that a matrix URL with a user name, which
// makes the service log in when it is located, is a configuration error
// without a request; the form with an access token alone is accepted, as
// it needs no login.
func TestNewRejectsMatrixLogin(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "http://")
	for _, url := range []string{"matrix://user:SECRET-PASSWORD@" + host + "/?disableTLS=yes", "MATRIX://user@" + host + "/?disableTLS=yes"} {
		_, err := New(Options{URL: url, Client: server.Client()})
		if err == nil || err.Error() != "matrix login by password is not supported, use an access token" {
			t.Errorf("New(%s) error = %v", url, err)
		}
	}
	if _, err := New(Options{URL: "matrix://:SECRET-TOKEN@" + host + "/?disableTLS=yes", Client: server.Client()}); err != nil {
		t.Errorf("New() with an access token: %v", err)
	}
	if n := requests.Load(); n != 0 {
		t.Errorf("server got %d requests, want none", n)
	}
}
