package webhook

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/6RUN0/slendmail/internal/backend"
	"github.com/6RUN0/slendmail/internal/golden"
	"github.com/6RUN0/slendmail/internal/render"
	"github.com/6RUN0/slendmail/internal/text"
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
	opts := Options{URL: "https://hooks.example.org/in", Headers: map[string]string{"Content-Type": "text/plain", "Authorization": "Bearer t0ken"}}
	req, err := buildRequest(context.Background(), opts, backend.Payload{Text: "{}"})
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
// break or NUL, which config.Load rejects, fails for good without a
// request when it reaches the sender anyway, and that the error does not
// quote the value.
func TestSendRejectsHeaderInjection(t *testing.T) {
	for name, value := range map[string]string{"crlf": "Bearer SECRET\r\nX-Evil: 1", "lf": "SECRET\nX-Evil: 1", "nul": "SECRET\x00"} {
		t.Run(name, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
			defer server.Close()
			opts := Options{URL: server.URL, Headers: map[string]string{"Authorization": value}, Client: server.Client()}
			if _, err := buildRequest(context.Background(), opts, backend.Payload{Text: "{}"}); err == nil {
				t.Error("buildRequest() accepted the header")
			}
			err := New(opts).Send(context.Background(), backend.Payload{Text: "{}"})
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
		})
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
