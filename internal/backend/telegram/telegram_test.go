package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/6RUN0/mailcrier/internal/backend"
	"github.com/6RUN0/mailcrier/internal/delivery"
	"github.com/6RUN0/mailcrier/internal/message"
	"github.com/6RUN0/mailcrier/internal/render"
	"github.com/6RUN0/mailcrier/internal/text"
)

// testToken is a bot token of the Bot API format; it must never show in
// an error.
const testToken = "123456:AAtestTOKENtestTOKENtestTOKEN0123"

func TestBuildRequest(t *testing.T) {
	cases := []struct {
		name string
		opts Options
		want map[string]any
	}{
		{"T-ADJ-44/defaults", Options{ChatID: "-100123"}, map[string]any{
			"chat_id": "-100123", "text": "<b>s</b>", "parse_mode": "HTML",
			"link_preview_options": map[string]any{"is_disabled": true},
		}},
		{"T-ADJ-44/thread-and-silent", Options{ChatID: "@ops", MessageThreadID: 42, DisableNotification: true}, map[string]any{
			"chat_id": "@ops", "text": "<b>s</b>", "parse_mode": "HTML", "message_thread_id": float64(42),
			"link_preview_options": map[string]any{"is_disabled": true}, "disable_notification": true,
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.opts.Token, tc.opts.APIURL = testToken, DefaultAPIURL
			req, err := buildRequest(context.Background(), tc.opts, backend.Payload{Title: "s", Text: "<b>s</b>"})
			if err != nil {
				t.Fatal(err)
			}
			if want := "https://api.telegram.org/bot" + testToken + "/sendMessage"; req.Method != http.MethodPost || req.URL.String() != want {
				t.Errorf("request %s %s, want POST %s", req.Method, req.URL, want)
			}
			if got := req.Header.Get("Content-Type"); got != "application/json" {
				t.Errorf("Content-Type = %q", got)
			}
			var got map[string]any
			if err := json.NewDecoder(req.Body).Decode(&got); err != nil {
				t.Fatal(err)
			}
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Errorf("body = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestParseResponse(t *testing.T) {
	cases := []struct {
		name           string
		status         int
		header         http.Header
		body           string
		wantClass      backend.Class
		wantStatus     int
		wantRetryAfter time.Duration
		wantText       string
	}{
		{"ok", 200, nil, `{"ok":true,"result":{"message_id":1}}`, 0, 0, 0, ""},
		{"T-ADJ-45/ok-false-with-200", 200, nil, `{"ok":false,"error_code":400,"description":"Bad Request: chat not found"}`, backend.Permanent, 400, 0, "Bad Request: chat not found"},
		{"T-ADJ-45/ok-false-with-400", 400, nil, `{"ok":false,"error_code":400,"description":"Bad Request: can't parse entities"}`, backend.Permanent, 400, 0, "can't parse entities"},
		{"blocked", 403, nil, `{"ok":false,"error_code":403,"description":"Forbidden: bot was blocked by the user"}`, backend.Permanent, 403, 0, "Forbidden"},
		{"retry-after-of-telegram", 429, nil, `{"ok":false,"error_code":429,"description":"Too Many Requests: retry after 7","parameters":{"retry_after":7}}`, backend.Temporary, 429, 7 * time.Second, "Too Many Requests"},
		{"retry-after-with-200", 200, nil, `{"ok":false,"error_code":400,"description":"Flood","parameters":{"retry_after":3}}`, backend.Temporary, 400, 3 * time.Second, "Flood"},
		{"retry-after-header", 429, http.Header{"Retry-After": {"9"}}, `{"ok":false,"error_code":429,"description":"Too Many Requests"}`, backend.Temporary, 429, 9 * time.Second, "Too Many Requests"},
		{"retry-after-bounded", 429, nil, `{"ok":false,"error_code":429,"description":"Too Many Requests","parameters":{"retry_after":9300000000}}`, backend.Temporary, 429, backend.MaxRetryAfter, "Too Many Requests"},
		{"json-without-ok-with-200", 200, nil, `{}`, backend.Temporary, 200, 0, "no description"},
		{"error-code-zero-with-200", 200, nil, `{"ok":false,"error_code":0,"description":"proxy said no"}`, backend.Temporary, 200, 0, "proxy said no"},
		{"json-without-ok-with-403", 403, nil, `{}`, backend.Permanent, 403, 0, "no description"},
		{"server-error", 502, nil, `{"ok":false,"error_code":502,"description":"Bad Gateway"}`, backend.Temporary, 502, 0, "Bad Gateway"},
		{"T-ADJ-45/answer-not-json", 200, nil, `<html>proxy</html>`, backend.Temporary, 200, 0, "answer is not JSON"},
		{"T-ADJ-45/error-page-not-json", 502, nil, `<html>bad gateway</html>`, backend.Temporary, 502, 0, "answer is not JSON"},
		{"error-page-not-json-rejected", 404, nil, `<html>not found</html>`, backend.Permanent, 404, 0, "answer is not JSON"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			header := tc.header
			if header == nil {
				header = http.Header{}
			}
			err := parseResponse(&http.Response{StatusCode: tc.status, Header: header, Body: io.NopCloser(strings.NewReader(tc.body))})
			if tc.wantClass == 0 {
				if err != nil {
					t.Fatalf("parseResponse() error = %v", err)
				}
				return
			}
			var deliveryErr *backend.Error
			if !errors.As(err, &deliveryErr) {
				t.Fatalf("parseResponse() error = %v, want *backend.Error", err)
			}
			if deliveryErr.Class != tc.wantClass || deliveryErr.Status != tc.wantStatus || deliveryErr.RetryAfter != tc.wantRetryAfter || !strings.Contains(err.Error(), tc.wantText) {
				t.Errorf("error = %+v (%v), want class %v status %d retry after %v text %q", deliveryErr, err, tc.wantClass, tc.wantStatus, tc.wantRetryAfter, tc.wantText)
			}
		})
	}
}

// TestSendTransportErrors pins distinct error texts for a timeout and a
// refused connection, both temporary and without the token. A timeout
// names the key and the value of the bound that ran out, also while the
// answer of a 4xx is read, which then is temporary: the answer is unknown.
func TestSendTransportErrors(t *testing.T) {
	release := make(chan struct{})
	hanging := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer hanging.Close()
	cutAnswer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"ok": false, "descr`))
		w.(http.Flusher).Flush()
		<-release
	}))
	defer cutAnswer.Close()
	// A proxy closes the connection in the middle of the answer.
	closedAnswer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		_, _ = buf.WriteString("HTTP/1.1 400 Bad Request\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{\"ok\": false, \"descr")
		_ = buf.Flush()
		_ = conn.Close()
	}))
	defer closedAnswer.Close()
	defer close(release)
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	// The deadline of the delivery runs out first only without the
	// timeout of the client.
	deadline := func(timeout time.Duration) (context.Context, context.CancelFunc) {
		limit := 100 * time.Millisecond
		if timeout > 0 {
			limit = time.Minute
		}
		return context.WithTimeoutCause(context.Background(), limit, &backend.LimitError{Key: "deadline", Value: limit})
	}
	cases := []struct {
		name     string
		url      string
		timeout  time.Duration
		wantText string
	}{
		{"T-ADJ-45/timeout", hanging.URL, 100 * time.Millisecond, "temporary failure: Post: http_timeout 100ms exceeded"},
		{"T-ADJ-45/deadline", hanging.URL, 0, "temporary failure: Post: deadline 100ms exceeded"},
		{"T-ADJ-45/answer-cut-by-timeout", cutAnswer.URL, 100 * time.Millisecond, "temporary failure, status 400: answer not read: http_timeout 100ms exceeded"},
		{"T-ADJ-45/answer-cut-by-close", closedAnswer.URL, 100 * time.Millisecond, "temporary failure, status 400: answer not read: unexpected EOF"},
		{"T-ADJ-45/connection-refused", closed.URL, 100 * time.Millisecond, "connection refused"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sender := New(Options{Token: testToken, ChatID: "1", APIURL: tc.url, Client: &http.Client{Timeout: tc.timeout}})
			ctx, cancel := deadline(tc.timeout)
			defer cancel()
			err := sender.Send(ctx, backend.Payload{Text: "x"})
			var deliveryErr *backend.Error
			if !errors.As(err, &deliveryErr) || deliveryErr.Class != backend.Temporary || !strings.Contains(err.Error(), tc.wantText) {
				t.Errorf("Send() error = %v, want temporary with %q", err, tc.wantText)
			}
			if err != nil && strings.Contains(err.Error(), testToken) {
				t.Errorf("error text contains the token: %v", err)
			}
		})
	}
}

// botAPI is a fake Bot API server. It checks the text of sendMessage the
// way Telegram does: only the tags and entities of the HTML parse mode,
// every tag closed, at most 4096 characters after entity parsing. A text
// that fails gets 400 with ok false.
type botAPI struct {
	server *httptest.Server
	// failDocuments makes sendDocument answer 413.
	failDocuments bool
	// rejectTexts makes sendMessage answer 400 to any text.
	rejectTexts bool

	mu        sync.Mutex
	texts     []string
	documents []document
	rejected  []string
}

type document struct {
	chatID, name, contentType, content, caption string
}

func newBotAPI(t *testing.T) *botAPI {
	t.Helper()
	api := &botAPI{}
	api.server = httptest.NewServer(http.HandlerFunc(api.serve))
	t.Cleanup(api.server.Close)
	return api
}

func (api *botAPI) sender(opts Options) *Sender {
	opts.Token, opts.APIURL, opts.Client = testToken, api.server.URL, api.server.Client()
	if opts.ChatID == "" {
		opts.ChatID = "-100123"
	}
	return New(opts)
}

func (api *botAPI) serve(w http.ResponseWriter, r *http.Request) {
	api.mu.Lock()
	defer api.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/bot" + testToken + "/sendMessage":
		var msg struct {
			Text      string `json:"text"`
			ParseMode string `json:"parse_mode"`
		}
		if err := json.NewDecoder(r.Body).Decode(&msg); err != nil || msg.ParseMode != "HTML" {
			http.Error(w, `{"ok":false,"error_code":400,"description":"Bad Request: no HTML"}`, http.StatusBadRequest)
			return
		}
		length, err := parseBotHTML(msg.Text)
		switch {
		case api.rejectTexts:
			api.rejected = append(api.rejected, "text")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"ok":false,"error_code":400,"description":"Bad Request: can't parse entities"}`)
			return
		case err != nil:
			api.rejected = append(api.rejected, err.Error())
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprintf(w, `{"ok":false,"error_code":400,"description":%q}`, "Bad Request: can't parse entities: "+err.Error())
			return
		case length == 0 || length > 4096:
			api.rejected = append(api.rejected, "length "+strconv.Itoa(length))
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"ok":false,"error_code":400,"description":"Bad Request: message is too long"}`)
			return
		}
		api.texts = append(api.texts, msg.Text)
	case "/bot" + testToken + "/sendDocument":
		if api.failDocuments {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			_, _ = io.WriteString(w, `{"ok":false,"error_code":413,"description":"Request Entity Too Large"}`)
			return
		}
		file, header, err := r.FormFile("document")
		if err != nil {
			http.Error(w, `{"ok":false,"error_code":400,"description":"Bad Request: no document"}`, http.StatusBadRequest)
			return
		}
		content, _ := io.ReadAll(file)
		caption := r.FormValue("caption")
		if length, err := parseBotHTML(caption); caption != "" && (err != nil || length > 1024 || r.FormValue("parse_mode") != "HTML") {
			api.rejected = append(api.rejected, fmt.Sprintf("caption of %d characters, err %v", length, err))
			http.Error(w, `{"ok":false,"error_code":400,"description":"Bad Request: message caption is too long"}`, http.StatusBadRequest)
			return
		}
		if len(content) == 0 {
			http.Error(w, `{"ok":false,"error_code":400,"description":"Bad Request: file must be non-empty"}`, http.StatusBadRequest)
			return
		}
		api.documents = append(api.documents, document{r.FormValue("chat_id"), header.Filename, header.Header.Get("Content-Type"), string(content), caption})
	default:
		http.Error(w, `{"ok":false,"error_code":404,"description":"Not Found"}`, http.StatusNotFound)
		return
	}
	_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":1}}`)
}

// botTags are the tags of the HTML parse mode.
var botTags = map[string]bool{
	"b": true, "strong": true, "i": true, "em": true, "u": true, "ins": true, "s": true, "strike": true, "del": true,
	"span": true, "tg-spoiler": true, "a": true, "tg-emoji": true, "code": true, "pre": true, "blockquote": true,
}

// botEntities are the named entities of the HTML parse mode.
var botEntities = map[string]rune{"lt": '<', "gt": '>', "amp": '&', "quot": '"'}

// parseBotHTML returns the length of the visible text in characters, or
// the error Telegram would report. It is written independently of
// text.MeasureTelegramHTML, which it checks.
func parseBotHTML(s string) (int, error) {
	var open []string
	length := 0
	for i := 0; i < len(s); {
		switch s[i] {
		case '<':
			end := strings.IndexByte(s[i:], '>')
			if end < 0 {
				return 0, fmt.Errorf("unclosed start tag at byte offset %d", i)
			}
			tag := s[i+1 : i+end]
			name, _, _ := strings.Cut(strings.TrimPrefix(tag, "/"), " ")
			if !botTags[strings.ToLower(name)] {
				return 0, fmt.Errorf("unsupported start tag %q at byte offset %d", name, i)
			}
			if strings.HasPrefix(tag, "/") {
				if len(open) == 0 || open[len(open)-1] != name {
					return 0, fmt.Errorf("unmatched end tag %q", name)
				}
				open = open[:len(open)-1]
			} else {
				open = append(open, name)
			}
			i += end + 1
		case '&':
			end := strings.IndexByte(s[i:], ';')
			if end < 0 {
				return 0, fmt.Errorf("unterminated entity at byte offset %d", i)
			}
			name := s[i+1 : i+end]
			_, ok := botEntities[name]
			if code, found := strings.CutPrefix(name, "#"); found {
				_, err := strconv.ParseUint(code, 10, 32)
				ok = err == nil
			}
			if !ok {
				return 0, fmt.Errorf("unsupported entity %q", name)
			}
			length++
			i += end + 1
		case '>':
			return 0, fmt.Errorf("bare > at byte offset %d", i)
		default:
			_, size := utf8.DecodeRuneInString(s[i:])
			length++
			i += size
		}
	}
	if len(open) > 0 {
		return 0, fmt.Errorf("unclosed tags %q", open)
	}
	return length, nil
}

// deliverTo renders d with the built-in telegram-html template and sends
// it with sender through delivery.Deliver, as a real invocation does.
func deliverTo(t *testing.T, sender *Sender, d render.Data, files []message.Attachment) delivery.Result {
	t.Helper()
	tmpl, err := render.Builtin(text.FormatTelegramHTML)
	if err != nil {
		t.Fatal(err)
	}
	d.Strings = render.DefaultStrings()
	return delivery.Deliver(context.Background(), []delivery.Target{{ID: "tg", Sender: sender, Template: tmpl}}, d, files)[0]
}

func TestDeliverTelegramHTML(t *testing.T) {
	t.Run("long-body-with-markup-and-cyrillic", func(t *testing.T) {
		api := newBotAPI(t)
		body := strings.Repeat("&<> ёжик & <b>x</b> 😀\n", 5000)
		if len(body) < 100<<10 {
			t.Fatalf("body of %d bytes, want 100 KiB or more", len(body))
		}
		result := deliverTo(t, api.sender(Options{}), render.Data{Subject: "disk <raid> & more", Hostname: "h", Body: body}, nil)
		if result.Status != delivery.OK || !result.IsTruncated || len(api.texts) != 1 {
			t.Fatalf("result = %+v, rejected: %q", result, api.rejected)
		}
		if n, _ := parseBotHTML(api.texts[0]); n > 4096 || n < 4000 || n != text.MeasureTelegramHTML(api.texts[0]) {
			t.Errorf("text of %d characters, MeasureTelegramHTML %d; want the same, close to 4096", n, text.MeasureTelegramHTML(api.texts[0]))
		}
	})
	t.Run("T-ESC-06/tags-outside-whitelist-literal", func(t *testing.T) {
		api := newBotAPI(t)
		result := deliverTo(t, api.sender(Options{}), render.Data{Subject: "s", Hostname: "h", Body: "<p>one</p><div>two</div><script>alert(1)</script>\n"}, nil)
		if result.Status != delivery.OK || len(api.texts) != 1 || !strings.Contains(api.texts[0], "&lt;script&gt;alert(1)&lt;/script&gt;") {
			t.Errorf("result = %+v, texts %q, rejected %q", result, api.texts, api.rejected)
		}
	})
	t.Run("T-ADJ-41/markup-characters-escaped", func(t *testing.T) {
		api := newBotAPI(t)
		d := render.Data{Subject: "a < b & c > d", Hostname: "h&h", From: message.Address{Name: "<Cron & Co>", Addr: "root"}, Body: "x < y && z > 0\n"}
		result := deliverTo(t, api.sender(Options{}), d, nil)
		if result.Status != delivery.OK || len(api.texts) != 1 {
			t.Fatalf("result = %+v, rejected %q", result, api.rejected)
		}
		for _, want := range []string{"a &lt; b &amp; c &gt; d", "h&amp;h", "&lt;Cron &amp; Co&gt; &lt;root&gt;", "x &lt; y &amp;&amp; z &gt; 0"} {
			if !strings.Contains(api.texts[0], want) {
				t.Errorf("text lacks %q: %q", want, api.texts[0])
			}
		}
	})
	t.Run("T-LIM-03/character-outside-bmp-counts-once", func(t *testing.T) {
		api := newBotAPI(t)
		body := strings.Repeat("😀", 4000)
		result := deliverTo(t, api.sender(Options{}), render.Data{Subject: "s", Hostname: "h", Body: body}, nil)
		if result.Status != delivery.OK || result.IsTruncated || len(api.texts) != 1 || !strings.Contains(api.texts[0], body) {
			t.Errorf("result = %+v, %d texts, rejected %q", result, len(api.texts), api.rejected)
		}
	})
	t.Run("T-LIM-02/limit-counts-characters-not-bytes", func(t *testing.T) {
		api := newBotAPI(t)
		body := strings.Repeat("ж", 4000)
		result := deliverTo(t, api.sender(Options{}), render.Data{Subject: "s", Hostname: "h", Body: body}, nil)
		if result.Status != delivery.OK || result.IsTruncated || len(api.texts) != 1 || !strings.Contains(api.texts[0], body) || len(api.texts[0]) <= 4096 {
			t.Errorf("result = %+v, %d texts, rejected %q", result, len(api.texts), api.rejected)
		}
	})
}

func TestDeliverTelegramDocuments(t *testing.T) {
	files := []message.Attachment{
		{Name: `report "1".log`, ContentType: "text/plain", Data: []byte("report one")},
		{Name: "dump.bin", ContentType: "application/octet-stream", Size: maxFileSize + 1, Data: make([]byte, maxFileSize+1)},
		{Name: "", Data: []byte("unnamed")},
	}
	d := render.Data{Subject: "s", Hostname: "h", Body: "b\n"}
	for _, f := range files {
		d.Attachments = append(d.Attachments, render.Attachment{Name: f.Name, ContentType: f.ContentType, Size: int64(len(f.Data))})
	}
	long := d
	long.Body = strings.Repeat("a long line of the body\n", 50)
	t.Run("T-LIM-07/file-over-50-mb-noted", func(t *testing.T) {
		api := newBotAPI(t)
		result := deliverTo(t, api.sender(Options{ChatID: "@ops"}), d, files)
		if result.Status != delivery.OK || result.Err != nil || len(api.documents) != 2 {
			t.Fatalf("result = %+v, rejected %q", result, api.rejected)
		}
		caption := api.documents[0].caption
		if !strings.Contains(caption, "dump.bin (application/octet-stream, 47.7 MiB) [not sent]") || strings.Count(caption, "[not sent]") != 1 {
			t.Errorf("text does not note the skipped file: %q", caption)
		}
		api.documents[0].caption = ""
		want := []document{{"@ops", `report "1".log`, "text/plain", "report one", ""}, {"@ops", "attachment-3", "application/octet-stream", "unnamed", ""}}
		if fmt.Sprint(api.documents) != fmt.Sprint(want) {
			t.Errorf("documents = %q, want %q", api.documents, want)
		}
	})
	t.Run("T-LIM-06/short-text-as-caption", func(t *testing.T) {
		api := newBotAPI(t)
		result := deliverTo(t, api.sender(Options{}), d, files)
		if result.Status != delivery.OK || len(api.texts) != 0 || len(api.documents) != 2 || !strings.HasPrefix(api.documents[0].caption, "<b>s</b>") || api.documents[1].caption != "" {
			t.Errorf("result = %+v, texts %q, documents %q", result, api.texts, api.documents)
		}
	})
	t.Run("T-LIM-06/long-text-then-documents-without-caption", func(t *testing.T) {
		api := newBotAPI(t)
		result := deliverTo(t, api.sender(Options{}), long, files)
		if result.Status != delivery.OK || len(api.texts) != 1 || len(api.documents) != 2 || api.documents[0].caption != "" || api.documents[1].caption != "" {
			t.Errorf("result = %+v, texts %q, documents %q", result, api.texts, api.documents)
		}
	})
	// The caption limit counts characters like the text limit: an emoji,
	// two UTF-16 units, counts once.
	for _, tc := range []struct {
		name        string
		length      int
		wantCaption bool
	}{
		{"T-LIM-06/caption-of-1024-characters", maxCaption, true},
		{"T-LIM-06/text-of-1025-characters-by-sendMessage", maxCaption + 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := newBotAPI(t)
			file := backend.Attachment{Name: "a.log", ContentType: "text/plain", Data: []byte("a")}
			payload := backend.Payload{Text: strings.Repeat("\U0001F600", tc.length), Attachments: []backend.Attachment{file}}
			if err := api.sender(Options{}).Send(context.Background(), payload); err != nil {
				t.Fatalf("Send() = %v", err)
			}
			isCaption := len(api.texts) == 0 && len(api.documents) == 1 && api.documents[0].caption == payload.Text
			isMessage := len(api.texts) == 1 && len(api.documents) == 1 && api.documents[0].caption == ""
			if tc.wantCaption && !isCaption || !tc.wantCaption && !isMessage {
				var captions []int
				for _, doc := range api.documents {
					captions = append(captions, utf8.RuneCountInString(doc.caption))
				}
				t.Errorf("texts %d, captions of %v characters", len(api.texts), captions)
			}
		})
	}
	t.Run("empty-first-file-without-caption", func(t *testing.T) {
		api := newBotAPI(t)
		empty := []message.Attachment{{Name: "empty.log", ContentType: "text/plain"}, files[0]}
		result := deliverTo(t, api.sender(Options{}), render.Data{Subject: "s", Hostname: "h", Body: "b\n"}, empty)
		var deliveryErr *backend.Error
		if result.Status != delivery.OK || len(api.texts) != 1 || !errors.As(result.Err, &deliveryErr) || !deliveryErr.IsPartial {
			t.Errorf("result = %+v, texts %q, documents %q", result, api.texts, api.documents)
		}
	})
	t.Run("document-failure-is-partial", func(t *testing.T) {
		api := newBotAPI(t)
		api.failDocuments = true
		result := deliverTo(t, api.sender(Options{}), long, files)
		var deliveryErr *backend.Error
		if result.Status != delivery.OK || !errors.As(result.Err, &deliveryErr) || !deliveryErr.IsPartial || deliveryErr.Status != 413 || len(api.texts) != 1 {
			t.Errorf("result = %+v", result)
		}
		if !strings.Contains(result.Err.Error(), "document 1 of 2") {
			t.Errorf("error %q does not name the document", result.Err)
		}
	})
	t.Run("caption-failure-is-not-partial", func(t *testing.T) {
		api := newBotAPI(t)
		api.failDocuments = true
		result := deliverTo(t, api.sender(Options{}), d, files)
		var deliveryErr *backend.Error
		if result.Status != delivery.Perm || !errors.As(result.Err, &deliveryErr) || deliveryErr.IsPartial || deliveryErr.IsTextRejected {
			t.Errorf("result = %+v", result)
		}
	})
}

// TestSendRejectedText pins which failures of the request with the text
// are IsTextRejected: 400, from sendMessage or from the document with the
// caption, and nothing else; and that an empty text sends the documents
// alone.
func TestSendRejectedText(t *testing.T) {
	file := backend.Attachment{Name: "message.txt", ContentType: "text/plain", Data: []byte("full text")}
	t.Run("T-ADJ-49/400-on-sendMessage", func(t *testing.T) {
		api := newBotAPI(t)
		api.rejectTexts = true
		err := api.sender(Options{}).Send(context.Background(), backend.Payload{Text: strings.Repeat("x", 2000), Attachments: []backend.Attachment{file}})
		var deliveryErr *backend.Error
		if !errors.As(err, &deliveryErr) || !deliveryErr.IsTextRejected || deliveryErr.IsPartial || len(api.documents) != 0 {
			t.Errorf("err = %+v, documents %q", err, api.documents)
		}
	})
	t.Run("400-on-caption", func(t *testing.T) {
		api := newBotAPI(t)
		err := api.sender(Options{}).Send(context.Background(), backend.Payload{Text: "<b>open", Attachments: []backend.Attachment{file}})
		var deliveryErr *backend.Error
		if !errors.As(err, &deliveryErr) || !deliveryErr.IsTextRejected {
			t.Errorf("err = %+v", err)
		}
	})
	t.Run("other-status-not-text-rejected", func(t *testing.T) {
		api := newBotAPI(t)
		err := New(Options{Token: "wrong", ChatID: "1", APIURL: api.server.URL, Client: api.server.Client()}).Send(context.Background(), backend.Payload{Text: "x"})
		var deliveryErr *backend.Error
		if !errors.As(err, &deliveryErr) || deliveryErr.Status != 404 || deliveryErr.IsTextRejected {
			t.Errorf("err = %+v", err)
		}
	})
	t.Run("empty-text-sends-documents-alone", func(t *testing.T) {
		api := newBotAPI(t)
		err := api.sender(Options{}).Send(context.Background(), backend.Payload{Attachments: []backend.Attachment{file, file}})
		if err != nil || len(api.texts) != 0 || len(api.documents) != 2 || api.documents[0].caption != "" {
			t.Errorf("err = %v, texts %q, documents %q", err, api.texts, api.documents)
		}
	})
	t.Run("empty-text-first-document-failure-not-partial", func(t *testing.T) {
		api := newBotAPI(t)
		api.failDocuments = true
		err := api.sender(Options{}).Send(context.Background(), backend.Payload{Attachments: []backend.Attachment{file}})
		var deliveryErr *backend.Error
		if !errors.As(err, &deliveryErr) || deliveryErr.IsPartial || deliveryErr.IsTextRejected {
			t.Errorf("err = %+v", err)
		}
	})
}

// deliverTarget sends d through delivery to target, whose template is the
// built-in telegram-html one.
func deliverTarget(t *testing.T, target delivery.Target, d render.Data, files []message.Attachment) delivery.Result {
	t.Helper()
	tmpl, err := render.Builtin(text.FormatTelegramHTML)
	if err != nil {
		t.Fatal(err)
	}
	target.ID, target.Template, d.Strings = "tg", tmpl, render.DefaultStrings()
	return delivery.Deliver(context.Background(), []delivery.Target{target}, d, files)[0]
}

// TestDeliverTelegramLongText pins the long text policy with the Bot API:
// what goes as text, what as message.txt, and the retry as a document.
func TestDeliverTelegramLongText(t *testing.T) {
	tmpl, err := render.Builtin(text.FormatTelegramHTML)
	if err != nil {
		t.Fatal(err)
	}
	empty := render.Data{Subject: "s", Hostname: "h", Strings: render.DefaultStrings()}
	wrapper, err := tmpl.Execute(empty)
	if err != nil {
		t.Fatal(err)
	}
	atLimit := strings.Repeat("x", maxText-text.MeasureTelegramHTML(wrapper)+text.RuneCount(empty.Strings.EmptyBody))
	t.Run("T-LIM-01/4096-characters-as-they-are", func(t *testing.T) {
		api := newBotAPI(t)
		result := deliverTo(t, api.sender(Options{}), render.Data{Subject: "s", Hostname: "h", Body: atLimit}, nil)
		if result.Status != delivery.OK || result.IsTruncated || len(api.texts) != 1 || len(api.documents) != 0 || text.MeasureTelegramHTML(api.texts[0]) != maxText {
			t.Errorf("result = %+v, %d texts, %d documents", result, len(api.texts), len(api.documents))
		}
	})
	t.Run("T-LIM-01/4097-characters-cut-and-file", func(t *testing.T) {
		api := newBotAPI(t)
		result := deliverTo(t, api.sender(Options{}), render.Data{Subject: "s", Hostname: "h", Body: atLimit + "y"}, nil)
		if result.Status != delivery.OK || !result.IsTruncated || len(api.texts) != 1 || len(api.documents) != 1 {
			t.Fatalf("result = %+v, %d texts, %d documents, rejected %q", result, len(api.texts), len(api.documents), api.rejected)
		}
		if doc := api.documents[0]; doc.name != "message.txt" || doc.caption != "" || !strings.Contains(doc.content, atLimit+"y") {
			t.Errorf("document %q of %d bytes, caption %q", doc.name, len(doc.content), doc.caption)
		}
		if !strings.Contains(api.texts[0], "[truncated]</pre>\nmessage.txt (text/plain") {
			t.Errorf("text ends %q", api.texts[0][max(0, len(api.texts[0])-80):])
		}
	})
	t.Run("T-ADJ-60/cut-before-send-not-rejected", func(t *testing.T) {
		api := newBotAPI(t)
		result := deliverTo(t, api.sender(Options{}), render.Data{Subject: "s", Hostname: "h", Body: strings.Repeat(atLimit, 3)}, nil)
		if result.Status != delivery.OK || !result.IsTruncated || result.TextRejected != nil || len(api.rejected) != 0 || len(api.texts) != 1 {
			t.Fatalf("result = %+v, %d texts, rejected %q", result, len(api.texts), api.rejected)
		}
		if n := text.MeasureTelegramHTML(api.texts[0]); n > maxText {
			t.Errorf("text of %d characters sent, limit %d", n, maxText)
		}
	})
	t.Run("T-CALL-26/2-mb-log-as-file", func(t *testing.T) {
		api := newBotAPI(t)
		body := strings.Repeat("Sep 27 03:00:01 host CRON[1234]: (root) CMD (run-parts /etc/cron.daily)\n", 2<<20/72)
		result := deliverTo(t, api.sender(Options{}), render.Data{Subject: "logwatch", Hostname: "h", Body: body}, nil)
		if result.Status != delivery.OK || !result.IsTruncated || len(api.texts) != 1 || len(api.documents) != 1 {
			t.Fatalf("result = %+v, %d texts, %d documents", result, len(api.texts), len(api.documents))
		}
		if doc := api.documents[0]; len(doc.content) < 2<<20-100 || !strings.Contains(doc.content, strings.TrimSpace(body)) {
			t.Errorf("message.txt of %d bytes, want the full log", len(doc.content))
		}
	})
	t.Run("T-ADJ-49/400-retried-once-as-document", func(t *testing.T) {
		api := newBotAPI(t)
		api.rejectTexts = true
		result := deliverTo(t, api.sender(Options{}), render.Data{Subject: "s", Hostname: "h", Body: "b\n"}, nil)
		var rejected *backend.Error
		if result.Status != delivery.OK || result.Err != nil || !errors.As(result.TextRejected, &rejected) || rejected.Status != 400 {
			t.Fatalf("result = %+v", result)
		}
		if len(api.rejected) != 1 || len(api.documents) != 1 || api.documents[0].name != "message.txt" || api.documents[0].caption != "" || !strings.Contains(api.documents[0].content, "s\nh: ") {
			t.Errorf("rejected %q, documents %q", api.rejected, api.documents)
		}
	})
	t.Run("T-ADJ-49/retry-fails-once", func(t *testing.T) {
		api := newBotAPI(t)
		api.rejectTexts, api.failDocuments = true, true
		files := []message.Attachment{{Name: "a.log", Data: []byte("a")}}
		result := deliverTo(t, api.sender(Options{}), render.Data{Subject: "s", Hostname: "h", Body: strings.Repeat("b ", 1000)}, files)
		var deliveryErr *backend.Error
		if result.Status != delivery.Perm || !errors.As(result.Err, &deliveryErr) || deliveryErr.Status != 413 || deliveryErr.IsPartial || result.TextRejected == nil || len(api.rejected) != 1 {
			t.Errorf("result = %+v, rejected %q", result, api.rejected)
		}
	})
	t.Run("blockquote-collapses-the-cut-body", func(t *testing.T) {
		api := newBotAPI(t)
		result := deliverTarget(t, delivery.Target{Sender: api.sender(Options{}), OnLong: delivery.OnLongBlockquote}, render.Data{Subject: "s", Hostname: "h", Body: atLimit + "y"}, nil)
		if result.Status != delivery.OK || len(api.texts) != 1 || len(api.documents) != 1 || !strings.Contains(api.texts[0], "<blockquote expandable>xxx") || !strings.Contains(api.texts[0], "[truncated]</blockquote>") {
			t.Errorf("result = %+v, texts %q", result, api.texts)
		}
	})
	t.Run("T-LIM-16/wrapper-over-max-text-cut-hard-and-file", func(t *testing.T) {
		api := newBotAPI(t)
		d := render.Data{Subject: "s", Hostname: strings.Repeat("host-&-", 100), Body: "b\n"}
		result := deliverTarget(t, delivery.Target{Sender: api.sender(Options{}), MaxText: 300}, d, nil)
		if result.Status != delivery.OK || !result.IsTruncated || len(api.texts) != 0 || len(api.documents) != 1 {
			t.Fatalf("result = %+v, rejected %q", result, api.rejected)
		}
		caption := api.documents[0].caption
		if n := text.MeasureTelegramHTML(caption); n > 300 || n < 250 || !strings.HasSuffix(caption, "</i>") {
			t.Errorf("caption of %d characters: %q", n, caption)
		}
		if !strings.Contains(api.documents[0].content, d.Hostname) {
			t.Errorf("message.txt lacks the host: %q", api.documents[0].content)
		}
	})
}

// FuzzTelegramText checks that any subject and body, fitted to the limit
// of the target, give a text the Bot API parses and accepts: known tags
// only, all closed, entities whole, at most 4096 characters, as
// MeasureTelegramHTML counts them.
func FuzzTelegramText(f *testing.F) {
	f.Add("disk <raid> & more", strings.Repeat("&<> ёжик 😀\n", 400))
	f.Add("", "")
	f.Add("&amp;", "<pre>&#128512;</pre>")
	tmpl, err := render.Builtin(text.FormatTelegramHTML)
	if err != nil {
		f.Fatal(err)
	}
	caps := (&Sender{}).Caps()
	f.Fuzz(func(t *testing.T, subject, body string) {
		if !utf8.ValidString(subject) || !utf8.ValidString(body) {
			return
		}
		d := render.Data{Subject: subject, Hostname: "h", Body: body, Strings: render.DefaultStrings()}
		out, _, err := render.Fit(tmpl, d, caps.MaxText, caps.Measure)
		if err != nil {
			t.Fatal(err)
		}
		length, err := parseBotHTML(out)
		if err != nil || length == 0 || length > caps.MaxText || length != caps.Measure(out) {
			t.Errorf("Bot API would reject %q: length %d, measured %d, err %v", out, length, caps.Measure(out), err)
		}
	})
}
