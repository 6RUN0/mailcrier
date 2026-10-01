package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/6RUN0/slendmail/internal/backend"
	"github.com/6RUN0/slendmail/internal/delivery"
	"github.com/6RUN0/slendmail/internal/message"
	"github.com/6RUN0/slendmail/internal/render"
	"github.com/6RUN0/slendmail/internal/text"
)

const testToken = "xoxb-1234-5678-T0KENabcdefghij"

// webAPI is a fake Slack Web API: it records the posted messages and the
// uploads, and answers ok for a request with the test token.
type webAPI struct {
	server *httptest.Server
	// failUpload makes the upload URL answer 500.
	failUpload bool
	// uploadBase, when set, replaces the server in the upload URLs.
	uploadBase string

	mu        sync.Mutex
	posts     []map[string]any
	uploads   []string
	completed []string
}

func newWebAPI(t *testing.T) *webAPI {
	t.Helper()
	api := &webAPI{}
	api.server = httptest.NewServer(http.HandlerFunc(api.serve))
	t.Cleanup(api.server.Close)
	return api
}

func (api *webAPI) sender() *Sender {
	return New(Options{Token: testToken, Channel: "#ops", APIURL: api.server.URL + "/api", Client: api.server.Client()})
}

func (api *webAPI) serve(w http.ResponseWriter, r *http.Request) {
	api.mu.Lock()
	defer api.mu.Unlock()
	if r.URL.Path == "/upload/F1" || r.URL.Path == "/upload/F2" {
		if api.failUpload {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		data, _ := io.ReadAll(r.Body)
		api.uploads = append(api.uploads, r.URL.Path+" "+string(data))
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+testToken {
		_, _ = io.WriteString(w, `{"ok":false,"error":"invalid_auth"}`)
		return
	}
	switch r.URL.Path {
	case "/api/chat.postMessage":
		var msg map[string]any
		_ = json.NewDecoder(r.Body).Decode(&msg)
		api.posts = append(api.posts, msg)
		_, _ = io.WriteString(w, `{"ok":true,"channel":"C123","ts":"1700000000.000100"}`)
	case "/api/files.getUploadURLExternal":
		id := fmt.Sprintf("F%d", len(api.completed)+len(api.uploads)+1)
		base := api.server.URL
		if api.uploadBase != "" {
			base = api.uploadBase
		}
		_, _ = fmt.Fprintf(w, `{"ok":true,"upload_url":"%s/upload/%s","file_id":"%s"}`, base, id, id)
	case "/api/files.completeUploadExternal":
		api.completed = append(api.completed, r.FormValue("files")+" "+r.FormValue("channel_id")+" "+r.FormValue("thread_ts"))
		_, _ = io.WriteString(w, `{"ok":true}`)
	default:
		_, _ = io.WriteString(w, `{"ok":false,"error":"unknown_method"}`)
	}
}

// deliverTo sends d through delivery with the slack-mrkdwn template.
func deliverTo(t *testing.T, sender *Sender, d render.Data, files []message.Attachment) delivery.Result {
	t.Helper()
	tmpl, err := render.Builtin(text.FormatSlackMrkdwn)
	if err != nil {
		t.Fatal(err)
	}
	d.Strings = render.DefaultStrings()
	return delivery.Deliver(context.Background(), []delivery.Target{{ID: "sl", Sender: sender, Template: tmpl}}, d, files)[0]
}

func TestDeliverSlack(t *testing.T) {
	t.Run("T-ESC-08/mentions-stay-text", func(t *testing.T) {
		api := newWebAPI(t)
		d := render.Data{Subject: "<!here> alert", Hostname: "h", Body: "<!channel> and <@U123> and <https://evil.example|click> & more\n"}
		result := deliverTo(t, api.sender(), d, nil)
		if result.Status != delivery.OK || len(api.posts) != 1 {
			t.Fatalf("result = %+v, posts %v", result, api.posts)
		}
		post := api.posts[0]
		got, _ := post["text"].(string)
		for _, want := range []string{"&lt;!here&gt; alert", "&lt;!channel&gt;", "&lt;@U123&gt;", "&lt;https://evil.example|click&gt; &amp; more"} {
			if !strings.Contains(got, want) {
				t.Errorf("text lacks %q: %q", want, got)
			}
		}
		if strings.ContainsAny(strings.NewReplacer("&lt;", "", "&gt;", "", "&amp;", "").Replace(got), "<>") {
			t.Errorf("text has a bare < or >: %q", got)
		}
		if post["channel"] != "#ops" || post["unfurl_links"] != false || post["unfurl_media"] != false {
			t.Errorf("post = %v", post)
		}
	})
	t.Run("files-in-thread", func(t *testing.T) {
		api := newWebAPI(t)
		files := []message.Attachment{{Name: "a.log", Data: []byte("one")}, {Name: "b.log", Data: []byte("two")}}
		result := deliverTo(t, api.sender(), render.Data{Subject: "s", Body: "b\n"}, files)
		if result.Status != delivery.OK || result.Err != nil {
			t.Fatalf("result = %+v", result)
		}
		wantUploads := "[/upload/F1 one /upload/F2 two]"
		wantCompleted := `[[{"id":"F1","title":"a.log"},{"id":"F2","title":"b.log"}] C123 1700000000.000100]`
		if fmt.Sprint(api.uploads) != wantUploads || fmt.Sprint(api.completed) != wantCompleted {
			t.Errorf("uploads %v, completed %v; want %s and %s", api.uploads, api.completed, wantUploads, wantCompleted)
		}
	})
	t.Run("T-LIM-11/long-text-cut-full-text-as-file", func(t *testing.T) {
		api := newWebAPI(t)
		body := strings.Repeat("a line of a long cron report\n", 2000)
		result := deliverTo(t, api.sender(), render.Data{Subject: "s", Body: body}, nil)
		if result.Status != delivery.OK || result.Err != nil || !result.IsTruncated || len(api.posts) != 1 {
			t.Fatalf("result = %+v", result)
		}
		got, _ := api.posts[0]["text"].(string)
		if n := text.UTF16Len(got); n > maxText || n < maxText-200 || !strings.Contains(got, "[truncated]") {
			t.Errorf("text of %d units", n)
		}
		if len(api.uploads) != 1 || !strings.HasPrefix(api.uploads[0], "/upload/F1 s\n") || !strings.Contains(api.uploads[0], strings.TrimSpace(body)) || !strings.Contains(fmt.Sprint(api.completed), `"title":"message.txt"`) {
			t.Errorf("%d uploads, completed %v; want message.txt with the full body", len(api.uploads), api.completed)
		}
	})
	t.Run("upload-failure-is-partial", func(t *testing.T) {
		api := newWebAPI(t)
		api.failUpload = true
		files := []message.Attachment{{Name: "a.log", Data: []byte("one")}}
		result := deliverTo(t, api.sender(), render.Data{Subject: "s", Body: "b\n"}, files)
		var deliveryErr *backend.Error
		if result.Status != delivery.OK || !errors.As(result.Err, &deliveryErr) || !deliveryErr.IsPartial || !strings.Contains(result.Err.Error(), "file 1 of 1") {
			t.Errorf("result = %+v", result)
		}
		if strings.Contains(result.Err.Error(), "/upload/") {
			t.Errorf("error quotes the upload URL: %v", result.Err)
		}
	})
}

// TestUploadTransportError pins that a failed connection to the signed
// upload URL is reported without the URL; the dialed host and port may
// show, the signed path must not.
func TestUploadTransportError(t *testing.T) {
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	api := newWebAPI(t)
	api.uploadBase = closed.URL
	result := deliverTo(t, api.sender(), render.Data{Subject: "s", Body: "b\n"}, []message.Attachment{{Name: "a.log", Data: []byte("one")}})
	var deliveryErr *backend.Error
	if result.Status != delivery.OK || !errors.As(result.Err, &deliveryErr) || !deliveryErr.IsPartial || deliveryErr.Class != backend.Temporary {
		t.Fatalf("result = %+v", result)
	}
	if msg := result.Err.Error(); strings.Contains(msg, "/upload/") || strings.Contains(msg, closed.URL) || !strings.Contains(msg, "connection refused") {
		t.Errorf("error %q quotes the upload URL or lacks the cause", msg)
	}
}

func TestParseResponse(t *testing.T) {
	cases := []struct {
		name           string
		status         int
		header         http.Header
		body           string
		wantClass      backend.Class
		wantRetryAfter time.Duration
		wantText       string
	}{
		{"ok", 200, nil, `{"ok":true}`, 0, 0, ""},
		{"ok-false-permanent", 200, nil, `{"ok":false,"error":"channel_not_found"}`, backend.Permanent, 0, "channel_not_found"},
		{"ok-false-rate-limited", 200, nil, `{"ok":false,"error":"ratelimited"}`, backend.Temporary, 0, "ratelimited"},
		{"rate-limited-status", 429, http.Header{"Retry-After": {"30"}}, `{"ok":false,"error":"ratelimited"}`, backend.Temporary, 30 * time.Second, "Too Many Requests"},
		{"server-error", 503, nil, `<html>`, backend.Temporary, 0, "Service Unavailable"},
		{"answer-not-json", 200, nil, `<html>`, backend.Temporary, 0, "answer is not JSON"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			header := tc.header
			if header == nil {
				header = http.Header{}
			}
			err := parseResponse(&http.Response{StatusCode: tc.status, Header: header, Body: io.NopCloser(strings.NewReader(tc.body))}, &answer{})
			if tc.wantClass == 0 {
				if err != nil {
					t.Fatalf("parseResponse() error = %v", err)
				}
				return
			}
			var deliveryErr *backend.Error
			if !errors.As(err, &deliveryErr) || deliveryErr.Class != tc.wantClass || deliveryErr.RetryAfter != tc.wantRetryAfter || !strings.Contains(err.Error(), tc.wantText) {
				t.Errorf("error = %+v (%v), want class %v retry after %v text %q", deliveryErr, err, tc.wantClass, tc.wantRetryAfter, tc.wantText)
			}
		})
	}
}

// TestSendKeepsTokenOutOfErrors pins that the bot token travels only in
// the Authorization header and shows in no error.
func TestSendKeepsTokenOutOfErrors(t *testing.T) {
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	err := New(Options{Token: testToken, Channel: "#ops", APIURL: closed.URL + "/api", Client: &http.Client{}}).Send(context.Background(), backend.Payload{Text: "x"})
	if err == nil || strings.Contains(err.Error(), testToken) {
		t.Errorf("Send() error = %v", err)
	}
	req, err := buildRequest(context.Background(), Options{Token: testToken, Channel: "#ops", APIURL: DefaultAPIURL}, backend.Payload{Text: "x"})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(req.Body)
	if strings.Contains(req.URL.String()+string(body), testToken) || req.URL.String() != "https://slack.com/api/chat.postMessage" {
		t.Errorf("request %s with body %s carries the token outside the header", req.URL, body)
	}
}
