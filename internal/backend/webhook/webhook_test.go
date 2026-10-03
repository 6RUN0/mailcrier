package webhook

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/6RUN0/mailcrier/internal/backend"
	"github.com/6RUN0/mailcrier/internal/golden"
	"github.com/6RUN0/mailcrier/internal/render"
	"github.com/6RUN0/mailcrier/internal/text"
)

func TestBuildRequestGenericJSON(t *testing.T) {
	opts := Options{URL: "https://hooks.example.org/in/abc"}
	tmpl, err := render.Builtin(text.FormatGenericJSON)
	if err != nil {
		t.Fatal(err)
	}
	document, err := tmpl.Execute(render.Data{Subject: "cron <root@db1> backup", Body: "line \"one\"\n\tline <two> & three\n", Hostname: "db1.example.org"})
	if err != nil {
		t.Fatal(err)
	}

	req, err := buildRequest(context.Background(), opts, backend.Payload{Text: document})
	if err != nil {
		t.Fatalf("buildRequest() error = %v", err)
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatal(err)
	}
	golden.Check(t, "testdata/generic-json.txtar", golden.Archive{
		Comment: "Request of the http target with the generic-json preset.\n",
		Sections: []golden.Section{
			{Name: "request", Data: []byte(req.Method + " " + req.URL.String() + "\nContent-Type: " + req.Header.Get("Content-Type") + "\n")},
			{Name: "body", Data: body},
		},
	})
}

// TestBuildRequestHeaders pins that configured headers are set after the
// Content-Type of the preset and override it.
func TestBuildRequestHeaders(t *testing.T) {
	opts := Options{URL: "https://hooks.example.org/in"}
	headers := map[string]string{"Content-Type": "text/plain", "Authorization": "Bearer t0ken"}
	req, err := buildRequest(context.Background(), opts, backend.Payload{Text: "{}", Request: &backend.Request{Headers: headers}})
	if err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Values("Content-Type"); len(got) != 1 || got[0] != "text/plain" {
		t.Errorf("Content-Type = %q, want only the configured one", got)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer t0ken" {
		t.Errorf("Authorization = %q", got)
	}
}

// TestBuildRequestMethod pins POST as the default and the configured
// method otherwise.
func TestBuildRequestMethod(t *testing.T) {
	for method, want := range map[string]string{"": http.MethodPost, http.MethodPut: http.MethodPut, http.MethodPatch: http.MethodPatch} {
		req, err := buildRequest(context.Background(), Options{URL: "https://hooks.example.org/in", Method: method}, backend.Payload{Text: "{}"})
		if err != nil {
			t.Fatal(err)
		}
		if req.Method != want {
			t.Errorf("method %q: request method = %s, want %s", method, req.Method, want)
		}
	}
}

// TestSendRejectsHeaderInjection pins that a header value with a line
// break or NUL, which a header template may render from the message,
// fails for good without a request, and that the error does not quote the
// value.
func TestSendRejectsHeaderInjection(t *testing.T) {
	t.Run("T-TPL-18/crlf", headerInjectionCase{"Bearer SECRET\r\nX-Evil: 1"}.check)
	t.Run("T-TPL-18/lf", headerInjectionCase{"SECRET\nX-Evil: 1"}.check)
	t.Run("T-TPL-18/nul", headerInjectionCase{"SECRET\x00"}.check)
}

// headerInjectionCase is a rendered header value the sender must refuse.
type headerInjectionCase struct {
	value string
}

func (c headerInjectionCase) check(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
	defer server.Close()
	opts := Options{URL: server.URL, Client: server.Client()}
	payload := backend.Payload{Text: "{}", Request: &backend.Request{Headers: map[string]string{"Authorization": c.value}}}
	if _, err := buildRequest(context.Background(), opts, payload); err == nil {
		t.Error("buildRequest() accepted the header")
	}
	err := New(opts).Send(context.Background(), payload)
	var deliveryErr *backend.Error
	if !errors.As(err, &deliveryErr) || deliveryErr.Class != backend.Permanent {
		t.Fatalf("Send() error = %v, want a permanent *backend.Error", err)
	}
	if requests != 0 {
		t.Errorf("server got %d requests, want none", requests)
	}
	if strings.Contains(err.Error(), "SECRET") {
		t.Errorf("error text quotes the header value: %v", err)
	}
}

// TestBuildRequestFields pins the members added to the document of the
// mattermost preset: username and channel, without escaping the text
// again.
func TestBuildRequestFields(t *testing.T) {
	tmpl, err := render.Builtin(text.FormatMattermost)
	if err != nil {
		t.Fatal(err)
	}
	document, err := tmpl.Execute(render.Data{Subject: "disk <md0> & more", Hostname: "h", Body: "b\n", Strings: render.DefaultStrings()})
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{URL: "https://mm.example.org/hooks/x", Fields: map[string]string{"username": "slendmail", "channel": "town-square"}}
	req, err := buildRequest(context.Background(), opts, backend.Payload{Text: document})
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatal(err)
	}
	golden.Check(t, "testdata/mattermost-fields.txtar", golden.Archive{
		Comment:  "Request of the http target with the mattermost preset, username and channel.\n",
		Sections: []golden.Section{{Name: "body", Data: body}},
	})
	for _, document := range []string{"[1]", "null", "\"text\""} {
		if _, err := buildRequest(context.Background(), opts, backend.Payload{Text: document}); err == nil || !strings.Contains(err.Error(), "not a JSON object") {
			t.Errorf("buildRequest(%s) error = %v, want not a JSON object", document, err)
		}
	}
}

func TestBuildRequestHidesURLInError(t *testing.T) {
	_, err := buildRequest(context.Background(), Options{URL: "https://example.org/hook/SECRET\x7f"}, backend.Payload{})
	if err == nil {
		t.Fatal("buildRequest() succeeded on an invalid URL")
	}
	if strings.Contains(err.Error(), "SECRET") {
		t.Errorf("error text contains the URL: %v", err)
	}
}

func TestParseResponse(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		wantErr    bool
		wantClass  backend.Class
		wantStatus int
	}{
		{"ok", http.StatusOK, false, 0, 0},
		{"no-content", http.StatusNoContent, false, 0, 0},
		{"server-error", http.StatusBadGateway, true, backend.Temporary, http.StatusBadGateway},
		{"rate-limited", http.StatusTooManyRequests, true, backend.Temporary, http.StatusTooManyRequests},
		{"rejected", http.StatusBadRequest, true, backend.Permanent, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := &http.Response{StatusCode: tc.status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("echo of the request"))}
			err := parseResponse(resp)
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("parseResponse() error = %v", err)
				}
				return
			}
			var deliveryErr *backend.Error
			if !errors.As(err, &deliveryErr) {
				t.Fatalf("parseResponse() error = %v, want *backend.Error", err)
			}
			if deliveryErr.Class != tc.wantClass || deliveryErr.Status != tc.wantStatus {
				t.Errorf("error = %+v, want class %v status %d", deliveryErr, tc.wantClass, tc.wantStatus)
			}
			if strings.Contains(err.Error(), "echo") {
				t.Errorf("error text contains the response body: %v", err)
			}
		})
	}
}

func TestParseResponseRetryAfter(t *testing.T) {
	resp := &http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{"Retry-After": {"120"}}, Body: http.NoBody}
	var deliveryErr *backend.Error
	if err := parseResponse(resp); !errors.As(err, &deliveryErr) || deliveryErr.Class != backend.Temporary || deliveryErr.RetryAfter != 2*time.Minute {
		t.Errorf("parseResponse() error = %+v, want temporary with RetryAfter 2m", err)
	}
}

func TestSendPostsToServer(t *testing.T) {
	var gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	sender := New(Options{URL: server.URL + "/hook", Client: server.Client()})
	want := `{"subject": "s", "body": "b", "hostname": "h"}`
	if err := sender.Send(context.Background(), backend.Payload{Title: "s", Text: want}); err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	if gotBody != want {
		t.Errorf("server got %s, want %s", gotBody, want)
	}
}

func TestSendClassifiesTransportErrorWithoutURL(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	endpoint := server.URL + "/hook/SECRET"
	server.Close()

	err := New(Options{URL: endpoint, Client: &http.Client{}}).Send(context.Background(), backend.Payload{})
	var deliveryErr *backend.Error
	if !errors.As(err, &deliveryErr) || deliveryErr.Class != backend.Temporary {
		t.Fatalf("Send() error = %v, want a temporary *backend.Error", err)
	}
	if strings.Contains(err.Error(), "SECRET") {
		t.Errorf("error text contains the URL: %v", err)
	}
}

func TestSendDoesNotFollowRedirect(t *testing.T) {
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		if r.URL.Path == "/hook" {
			http.Redirect(w, r, "/login", http.StatusFound)
		}
	}))
	defer server.Close()

	err := New(Options{URL: server.URL + "/hook", Client: server.Client()}).Send(context.Background(), backend.Payload{Title: "s", Text: "b"})
	var deliveryErr *backend.Error
	if !errors.As(err, &deliveryErr) {
		t.Fatalf("Send() error = %v, want *backend.Error", err)
	}
	if deliveryErr.Class != backend.Permanent || deliveryErr.Status != http.StatusFound {
		t.Errorf("error = %+v, want permanent with status 302", deliveryErr)
	}
	if !strings.Contains(err.Error(), "302") {
		t.Errorf("error text %q lacks the status code", err)
	}
	if len(requests) != 1 || requests[0] != "POST /hook" {
		t.Errorf("server got %v, want exactly [POST /hook]", requests)
	}
}

// endlessBody is a response body that never ends and counts what is read.
type endlessBody struct{ read int }

func (b *endlessBody) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	b.read += len(p)
	return len(p), nil
}

func (*endlessBody) Close() error { return nil }

func TestSendBoundsBody(t *testing.T) {
	t.Run("T-ADJ-50/error-body-bounded", func(t *testing.T) {
		body := &endlessBody{}
		client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusInternalServerError, Body: body, Header: http.Header{}}, nil
		})}
		err := New(Options{URL: "https://hooks.example.org/in", Client: client}).Send(context.Background(), backend.Payload{Text: "{}"})
		if err == nil {
			t.Fatal("Send() accepted status 500")
		}
		if body.read > backend.MaxDrainBytes {
			t.Errorf("read %d bytes of the body, want at most %d", body.read, backend.MaxDrainBytes)
		}
		if strings.Contains(err.Error(), "xxxx") {
			t.Errorf("error text quotes the body: %v", err)
		}
	})
}

// roundTripFunc answers requests without a network.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// TestCapsPerPreset pins the length limit of each preset: 40000 UTF-16
// units for slack-webhook, none for mattermost and generic-json.
func TestCapsPerPreset(t *testing.T) {
	for format, want := range map[text.Format]int{text.FormatSlackWebhook: 40000, text.FormatMattermost: 0, text.FormatGenericJSON: 0} {
		caps := New(Options{Format: format, Client: &http.Client{}}).Caps()
		if caps.MaxText != want || caps.MaxFiles != 0 || (want > 0) != (caps.Measure != nil) {
			t.Errorf("%s: Caps = %+v, want MaxText %d", format, caps, want)
		}
	}
}

// TestBuildRequestParts pins how the rendered path and query join the
// configured URL: the path after its path, the query after its query,
// scheme, host, port and user information unchanged.
func TestBuildRequestParts(t *testing.T) {
	for _, tc := range []struct {
		name, url, path string
		query           url.Values
		want            string
	}{
		{"path-after-path", "https://user:pw@hooks.example.org:8443/in/", "/x/a%20b%2Fc", nil, "https://user:pw@hooks.example.org:8443/in/x/a%20b%2Fc"},
		{"path-keeps-query", "https://hooks.example.org/in?token=abc", "/x", nil, "https://hooks.example.org/in/x?token=abc"},
		{"path-on-bare-host", "https://hooks.example.org", "/x", nil, "https://hooks.example.org/x"},
		{"T-TPL-17/query-encoded-and-added", "https://hooks.example.org/in?token=a+b", "", url.Values{"msg": {"a&b=c d/é"}}, "https://hooks.example.org/in?token=a+b&msg=a%26b%3Dc+d%2F%C3%A9"},
		{"T-TPL-17/query-on-url-without-query", "https://hooks.example.org/in", "", url.Values{"a": {"1", "2"}, "b": {""}}, "https://hooks.example.org/in?a=1&a=2&b="},
		{"escaped-slash-kept", "https://hooks.example.org/in%2Fx", "/a b%2Fc;v=1", nil, "https://hooks.example.org/in%2Fx/a%20b%2Fc;v=1"},
		{"escaped-percent-allowed", "https://hooks.example.org/in", "/100%25/a%25zz", nil, "https://hooks.example.org/in/100%25/a%25zz"},
		{"empty-path-appends-nothing", "https://hooks.example.org/in", "", url.Values{"a": {"1"}}, "https://hooks.example.org/in?a=1"},
		{"nothing-to-add", "https://hooks.example.org/in?x=%41", "", nil, "https://hooks.example.org/in?x=%41"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := buildRequest(context.Background(), Options{URL: tc.url}, backend.Payload{Text: "{}", Request: &backend.Request{Path: tc.path, Query: tc.query}})
			if err != nil || req.URL.String() != tc.want {
				t.Errorf("URL = %v, %v, want %s", req, err, tc.want)
			}
		})
	}
}

// pathCase is a rendered path the sender must refuse.
type pathCase struct {
	path string
}

func (c pathCase) check(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
	defer server.Close()
	err := New(Options{URL: server.URL + "/in", Client: server.Client()}).Send(context.Background(), backend.Payload{Text: "{}", Request: &backend.Request{Path: c.path}})
	var deliveryErr *backend.Error
	if !errors.As(err, &deliveryErr) || deliveryErr.Class != backend.Permanent || requests != 0 {
		t.Fatalf("Send() = %v, %d requests, want a permanent failure without a request", err, requests)
	}
	if strings.Contains(err.Error(), "evil") || strings.Contains(err.Error(), "127.0.0.1") {
		t.Errorf("error text quotes the path or the URL: %v", err)
	}
}

// TestSendRejectsPath pins the paths that would leave the configured host
// or path, or smuggle a query, rejected before any request.
func TestSendRejectsPath(t *testing.T) {
	t.Run("T-TPL-16/other-host", pathCase{"//evil.example/"}.check)
	t.Run("T-TPL-16/url-with-host", pathCase{"http://evil.example/x"}.check)
	t.Run("T-TPL-16/no-leading-slash", pathCase{"evil.example/x"}.check)
	t.Run("T-TPL-16/userinfo-like", pathCase{"@evil.example/x"}.check)
	t.Run("T-TPL-16/dot-dot", pathCase{"/a/../evil"}.check)
	t.Run("T-TPL-16/dot", pathCase{"/a/./evil"}.check)
	t.Run("T-TPL-16/escaped-dot-dot", pathCase{"/a/%2e%2E/evil"}.check)
	t.Run("T-TPL-16/escaped-slash-dot-dot", pathCase{"/a%2F..%2Fevil"}.check)
	t.Run("T-TPL-16/backslash", pathCase{"/a\\..\\evil"}.check)
	t.Run("T-TPL-16/query", pathCase{"/a?evil=1"}.check)
	t.Run("T-TPL-16/fragment", pathCase{"/a#evil"}.check)
	t.Run("T-TPL-16/control-character", pathCase{"/a%0Aevil"}.check)
	t.Run("T-TPL-16/bad-escape", pathCase{"/evil%zz"}.check)
	t.Run("T-TPL-16/dot-dot-with-parameter", pathCase{"/x/..;/evil"}.check)
	t.Run("T-TPL-16/double-escaped-dot-dot", pathCase{"/x/%252e%252e/evil"}.check)
	t.Run("T-TPL-16/double-escaped-slash-dot-dot", pathCase{"/x/a%252F..%252Fevil"}.check)
	t.Run("T-TPL-16/double-escaped-dot-dot-bad-parameter", pathCase{"/x/%252e%252e;%25zz/evil"}.check)
}

// TestBuildRequestGet pins that a GET carries neither a body nor a
// Content-Type, and that a GET target has no text limit.
func TestBuildRequestGet(t *testing.T) {
	t.Run("T-TPL-19/no-body", func(t *testing.T) {
		req, err := buildRequest(context.Background(), Options{URL: "https://hooks.example.org/in", Method: http.MethodGet},
			backend.Payload{Text: "ignored", Request: &backend.Request{Query: url.Values{"m": {"x"}}}})
		if err != nil {
			t.Fatal(err)
		}
		if req.Body != nil || req.ContentLength != 0 || req.Header.Get("Content-Type") != "" || req.URL.RawQuery != "m=x" {
			t.Errorf("request = %+v", req)
		}
	})
	t.Run("no-limit", func(t *testing.T) {
		if caps := New(Options{Method: http.MethodGet, Format: text.FormatSlackWebhook, Client: &http.Client{}}).Caps(); caps.MaxText != 0 {
			t.Errorf("Caps = %+v", caps)
		}
	})
}
