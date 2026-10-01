package ntfy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/6RUN0/slendmail/internal/backend"
	"github.com/6RUN0/slendmail/internal/delivery"
	"github.com/6RUN0/slendmail/internal/message"
	"github.com/6RUN0/slendmail/internal/render"
	"github.com/6RUN0/slendmail/internal/text"
)

func TestNew(t *testing.T) {
	cases := []struct {
		url, wantRoot, wantTopic string
	}{
		{"https://ntfy.sh/alerts", "https://ntfy.sh/", "alerts"},
		{"https://ntfy.example.org/sub/alerts/", "https://ntfy.example.org/sub/", "alerts"},
		{"https://user:pass@ntfy.example.org/alerts?auth=tk", "https://user:pass@ntfy.example.org/?auth=tk", "alerts"},
	}
	for _, tc := range cases {
		s, err := New(Options{URL: tc.url, Client: &http.Client{}})
		if err != nil || s.root.String() != tc.wantRoot || s.topic != tc.wantTopic {
			t.Errorf("New(%q): root %v, topic %q, err %v; want %q and %q", tc.url, s.root, s.topic, err, tc.wantRoot, tc.wantTopic)
		}
	}
	for _, bad := range []string{"https://ntfy.sh", "https://ntfy.sh/"} {
		if _, err := New(Options{URL: bad, Client: &http.Client{}}); err == nil {
			t.Errorf("New(%q) accepted a URL without topic", bad)
		}
	}
}

// server is a fake ntfy server that records publishes and attachments.
type server struct {
	*httptest.Server
	// status, when set, answers every request; putStatus every PUT.
	status, putStatus int

	mu          sync.Mutex
	publishes   []publish
	attachments []string
}

func newServer(t *testing.T) *server {
	t.Helper()
	s := &server{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.status != 0 {
			w.WriteHeader(s.status)
			return
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/":
			var p publish
			if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			s.publishes = append(s.publishes, p)
		case r.Method == http.MethodPut && s.putStatus != 0:
			w.WriteHeader(s.putStatus)
		case r.Method == http.MethodPut && r.URL.Path == "/alerts":
			data, _ := io.ReadAll(r.Body)
			s.attachments = append(s.attachments, r.URL.Query().Get("filename")+"|"+r.URL.Query().Get("title")+"|"+string(data))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *server) sender(t *testing.T) *Sender {
	t.Helper()
	sender, err := New(Options{URL: s.URL + "/alerts", Client: s.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return sender
}

func deliverTo(t *testing.T, sender *Sender, d render.Data, files []message.Attachment) delivery.Result {
	t.Helper()
	tmpl, err := render.Builtin(text.FormatNtfy)
	if err != nil {
		t.Fatal(err)
	}
	d.Strings = render.DefaultStrings()
	return delivery.Deliver(context.Background(), []delivery.Target{{ID: "nt", Sender: sender, Template: tmpl}}, d, files)[0]
}

func TestDeliverNtfy(t *testing.T) {
	t.Run("T-ESC-11/line-breaks-in-title", func(t *testing.T) {
		s := newServer(t)
		files := []message.Attachment{{Name: "ёжик.log", Data: []byte("one")}}
		result := deliverTo(t, s.sender(t), render.Data{Subject: "RAID\r\n degraded\non md0", Body: "b\n"}, files)
		if result.Status != delivery.OK || result.Err != nil || len(s.publishes) != 1 {
			t.Fatalf("result = %+v", result)
		}
		if got := s.publishes[0].Title; got != "RAID  degraded on md0" {
			t.Errorf("title = %q", got)
		}
		if want := "ёжик.log|RAID  degraded on md0|one"; len(s.attachments) != 1 || s.attachments[0] != want {
			t.Errorf("attachments = %q, want %q", s.attachments, want)
		}
	})
	t.Run("T-LIM-13/title-cut-to-1-kb", func(t *testing.T) {
		s := newServer(t)
		deliverTo(t, s.sender(t), render.Data{Subject: strings.Repeat("ж", 2000), Body: "b\n"}, nil)
		if got := s.publishes[0].Title; len(got) > maxTitle || len(got) < maxTitle-1 || !utf8.ValidString(got) {
			t.Errorf("title of %d bytes", len(got))
		}
	})
	t.Run("missing-subject-notice", func(t *testing.T) {
		s := newServer(t)
		deliverTo(t, s.sender(t), render.Data{Body: "b\n"}, nil)
		if p := s.publishes[0]; p.Title != "(no subject)" || p.Topic != "alerts" || p.Message != "b" {
			t.Errorf("publish = %+v", p)
		}
	})
	t.Run("T-LIM-13/long-message-as-attachment", func(t *testing.T) {
		s := newServer(t)
		body := strings.Repeat("ёжик ", 2000)
		result := deliverTo(t, s.sender(t), render.Data{Subject: "s", Body: body}, nil)
		m := s.publishes[0].Message
		if !result.IsTruncated || len(m) > maxText || len(m) < maxText-200 || !strings.Contains(m, "[truncated]\nmessage.txt (text/plain") {
			t.Errorf("message of %d bytes, truncated %v, ends %q", len(m), result.IsTruncated, m[max(0, len(m)-80):])
		}
		if len(s.attachments) != 1 || !strings.HasPrefix(s.attachments[0], "message.txt|s|s\n") || !strings.Contains(s.attachments[0], strings.TrimSpace(body)) {
			t.Errorf("%d attachments, want message.txt with the full body", len(s.attachments))
		}
	})
	t.Run("attachment-failure-is-partial", func(t *testing.T) {
		s := newServer(t)
		s.putStatus = http.StatusRequestEntityTooLarge
		result := deliverTo(t, s.sender(t), render.Data{Subject: "s", Body: "b\n"}, []message.Attachment{{Name: "a.log", Data: []byte("one")}})
		var deliveryErr *backend.Error
		if result.Status != delivery.OK || !errors.As(result.Err, &deliveryErr) || !deliveryErr.IsPartial || deliveryErr.Status != 413 || !strings.Contains(result.Err.Error(), "attachment 1 of 1") {
			t.Errorf("result = %+v", result)
		}
	})
	t.Run("rejected", func(t *testing.T) {
		s := newServer(t)
		s.status = http.StatusTooManyRequests
		result := deliverTo(t, s.sender(t), render.Data{Subject: "s", Body: "b\n"}, nil)
		if result.Status != delivery.Temp {
			t.Errorf("result = %+v", result)
		}
	})
}
