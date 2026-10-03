package discord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/6RUN0/mailcrier/internal/backend"
	"github.com/6RUN0/mailcrier/internal/delivery"
	"github.com/6RUN0/mailcrier/internal/message"
	"github.com/6RUN0/mailcrier/internal/render"
	"github.com/6RUN0/mailcrier/internal/text"
)

// webhookURL has the shape of a Discord webhook URL; its last segment is
// the token.
const webhookURL = "https://discord.com/api/webhooks/123/T0KEN-abcdefghijklmnop"

// decodeRequest returns the JSON message of req and the names and
// contents of its files, from either body form.
func decodeRequest(t *testing.T, req *http.Request) (map[string]any, map[string]string) {
	t.Helper()
	mediaType, params, err := mime.ParseMediaType(req.Header.Get("Content-Type"))
	if err != nil {
		t.Fatal(err)
	}
	var msg map[string]any
	files := map[string]string{}
	if mediaType == "application/json" {
		if err := json.NewDecoder(req.Body).Decode(&msg); err != nil {
			t.Fatal(err)
		}
		return msg, files
	}
	form := multipart.NewReader(req.Body, params["boundary"])
	for {
		part, err := form.NextPart()
		if errors.Is(err, io.EOF) {
			return msg, files
		}
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(part)
		if part.FormName() == "payload_json" {
			if err := json.Unmarshal(data, &msg); err != nil {
				t.Fatal(err)
			}
			continue
		}
		files[part.FormName()+" "+part.FileName()+" "+part.Header.Get("Content-Type")] = string(data)
	}
}

func TestBuildRequest(t *testing.T) {
	files := []backend.Attachment{{Name: "a.log", ContentType: "text/plain", Data: []byte("one")}, {Name: "b.bin", Data: []byte("two")}}
	t.Run("T-ESC-09/text-only", buildRequestCase{webhookURL, nil, "wait=true", "map[]"}.check)
	t.Run("T-ESC-09/with-files", buildRequestCase{webhookURL, files, "wait=true", "map[files[0] a.log text/plain:one files[1] b.bin application/octet-stream:two]"}.check)
	t.Run("T-LIM-10/thread-query-kept", buildRequestCase{webhookURL + "?thread_id=77", nil, "thread_id=77&wait=true", "map[]"}.check)
	t.Run("T-LIM-10/wait-not-doubled", buildRequestCase{webhookURL + "?wait=false", nil, "wait=true", "map[]"}.check)
}

type buildRequestCase struct {
	url       string
	files     []backend.Attachment
	wantQuery string
	wantFiles string
}

func (tc buildRequestCase) check(t *testing.T) {
	req, err := buildRequest(context.Background(), Options{URL: tc.url}, backend.Payload{Text: "@everyone <@123> <@&456> @here", Attachments: tc.files})
	if err != nil {
		t.Fatal(err)
	}
	if req.Method != http.MethodPost || req.URL.Path != "/api/webhooks/123/T0KEN-abcdefghijklmnop" || req.URL.RawQuery != tc.wantQuery {
		t.Errorf("request %s %s, want POST with query %q", req.Method, req.URL, tc.wantQuery)
	}
	msg, gotFiles := decodeRequest(t, req)
	if got := fmt.Sprint(msg["allowed_mentions"]); got != "map[parse:[]]" {
		t.Errorf("allowed_mentions = %s, want an empty parse list", got)
	}
	if msg["content"] != "@everyone <@123> <@&456> @here" {
		t.Errorf("content = %v", msg["content"])
	}
	if got := fmt.Sprint(gotFiles); got != tc.wantFiles {
		t.Errorf("files = %s, want %s", got, tc.wantFiles)
	}
	if len(tc.files) > 0 && fmt.Sprint(msg["attachments"]) != "[map[filename:a.log id:0] map[filename:b.bin id:1]]" {
		t.Errorf("attachments = %v", msg["attachments"])
	}
}

// TestSendStatuses pins the classes of the webhook answers.
func TestSendStatuses(t *testing.T) {
	t.Run("T-LIM-10/ok-with-message", sendStatusCase{http.StatusOK, nil, 0, 0}.check)
	t.Run("T-LIM-10/no-content", sendStatusCase{http.StatusNoContent, nil, 0, 0}.check)
	t.Run("T-LIM-10/bad-request-permanent", sendStatusCase{http.StatusBadRequest, nil, backend.Permanent, 0}.check)
	t.Run("T-LIM-10/unknown-webhook-permanent", sendStatusCase{http.StatusNotFound, nil, backend.Permanent, 0}.check)
	t.Run("T-LIM-10/payload-too-large-permanent", sendStatusCase{http.StatusRequestEntityTooLarge, nil, backend.Permanent, 0}.check)
	t.Run("T-LIM-10/rate-limited-temporary", sendStatusCase{http.StatusTooManyRequests, http.Header{"Retry-After": {"3"}}, backend.Temporary, 3 * time.Second}.check)
	t.Run("T-LIM-10/server-error-temporary", sendStatusCase{http.StatusBadGateway, nil, backend.Temporary, 0}.check)
}

type sendStatusCase struct {
	status         int
	header         http.Header
	wantClass      backend.Class
	wantRetryAfter time.Duration
}

func (tc sendStatusCase) check(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for key, values := range tc.header {
			w.Header()[key] = values
		}
		w.WriteHeader(tc.status)
		_, _ = fmt.Fprintf(w, `{"message": "echo %s"}`, r.URL)
	}))
	defer server.Close()
	err := New(Options{URL: server.URL + "/api/webhooks/1/T0KEN", Client: server.Client()}).Send(context.Background(), backend.Payload{Text: "x"})
	if tc.wantClass == 0 {
		if err != nil {
			t.Fatalf("Send() error = %v", err)
		}
		return
	}
	var deliveryErr *backend.Error
	if !errors.As(err, &deliveryErr) || deliveryErr.Class != tc.wantClass || deliveryErr.Status != tc.status || deliveryErr.RetryAfter != tc.wantRetryAfter {
		t.Errorf("Send() error = %+v, want class %v status %d retry after %v", err, tc.wantClass, tc.status, tc.wantRetryAfter)
	}
	if strings.Contains(err.Error(), "T0KEN") {
		t.Errorf("error text contains the token: %v", err)
	}
}

// TestDeliverDiscordFiles pins the file limits through delivery: at most 10
// files, each within 20 MiB and all within 25 MiB; the rest are noted.
func TestDeliverDiscordFiles(t *testing.T) {
	var files []message.Attachment
	d := render.Data{Subject: "s", Hostname: "h", Body: "b\n", Strings: render.DefaultStrings()}
	add := func(name string, size int) {
		files = append(files, message.Attachment{Name: name, ContentType: "text/plain", Data: make([]byte, size)})
		d.Attachments = append(d.Attachments, render.Attachment{Name: name, ContentType: "text/plain", Size: int64(size)})
	}
	add("big.log", maxFileSize+1)
	for i := range 11 {
		add(fmt.Sprintf("f%02d.log", i), 10)
	}
	var got []string
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		msg, sent := decodeRequest(t, r)
		for name := range sent {
			got = append(got, name)
		}
		if content, _ := msg["content"].(string); strings.Count(content, `\[not sent\]`) != 2 || text.UTF16Len(content) > maxText {
			t.Errorf("content does not note the two skipped files: %q", content)
		}
	}))
	defer server.Close()
	tmpl, err := render.Builtin(text.FormatDiscord)
	if err != nil {
		t.Fatal(err)
	}
	sender := New(Options{URL: server.URL + "/api/webhooks/1/T", Client: server.Client()})
	t.Run("T-LIM-09/ten-files-within-size", func(t *testing.T) {
		result := delivery.Deliver(context.Background(), []delivery.Target{{ID: "dc", Sender: sender, Template: tmpl}}, d, files)[0]
		if result.Status != delivery.OK || len(got) != maxFiles {
			t.Errorf("result = %+v, %d files sent, want %d", result, len(got), maxFiles)
		}
		for _, name := range got {
			if strings.Contains(name, "big.log") || strings.Contains(name, "f10.log") {
				t.Errorf("sent %s", name)
			}
		}
	})
}

// TestDeliverDiscordLongText pins the long text policy for Discord: the
// content is cut to 2000 characters and the full text goes as message.txt
// in the same request.
func TestDeliverDiscordLongText(t *testing.T) {
	t.Run("T-LIM-08/long-content-cut-full-text-as-file", func(t *testing.T) {
		body := strings.Repeat("disk full on /var ", 300)
		var content string
		var files map[string]string
		server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			var msg map[string]any
			msg, files = decodeRequest(t, r)
			content, _ = msg["content"].(string)
		}))
		defer server.Close()
		tmpl, err := render.Builtin(text.FormatDiscord)
		if err != nil {
			t.Fatal(err)
		}
		sender := New(Options{URL: server.URL + "/api/webhooks/1/T", Client: server.Client()})
		d := render.Data{Subject: "s", Hostname: "h", Body: body, Strings: render.DefaultStrings()}
		result := delivery.Deliver(context.Background(), []delivery.Target{{ID: "dc", Sender: sender, Template: tmpl}}, d, nil)[0]
		if result.Status != delivery.OK || !result.IsTruncated {
			t.Fatalf("result = %+v", result)
		}
		if n := text.UTF16Len(content); n > maxText || n < maxText-200 || !strings.Contains(content, "[truncated]\n```") || !strings.Contains(content, "message.txt (text/plain") {
			t.Errorf("content of %d units: %q", n, content[max(0, len(content)-120):])
		}
		full := files["files[0] message.txt text/plain; charset=utf-8"]
		if len(files) != 1 || !strings.Contains(full, strings.TrimSpace(body)) {
			t.Errorf("files %d, message.txt of %d bytes, want the full body", len(files), len(full))
		}
	})
}
