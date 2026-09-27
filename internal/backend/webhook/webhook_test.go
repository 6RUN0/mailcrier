package webhook

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/6RUN0/slendmail/internal/backend"
	"github.com/6RUN0/slendmail/internal/golden"
)

func TestBuildRequestGenericJSON(t *testing.T) {
	opts := Options{URL: "https://hooks.example.org/in/abc", Hostname: "db1.example.org"}
	payload := backend.Payload{Title: "cron <root@db1> backup", Text: "line \"one\"\n\tline <two> & three\n"}

	req, err := buildRequest(context.Background(), opts, payload)
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
			resp := &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader("echo of the request"))}
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

func TestSendPostsToServer(t *testing.T) {
	var gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	sender := New(Options{URL: server.URL + "/hook", Hostname: "h", Client: server.Client()})
	if err := sender.Send(context.Background(), backend.Payload{Title: "s", Text: "b"}); err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	if want := `{"subject":"s","body":"b","hostname":"h"}`; gotBody != want {
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

func TestParseResponseBoundsBody(t *testing.T) {
	t.Run("T-ADJ-50/error-body-bounded", func(t *testing.T) {
		body := &endlessBody{}
		err := parseResponse(&http.Response{StatusCode: http.StatusInternalServerError, Body: body})
		if err == nil {
			t.Fatal("parseResponse() accepted status 500")
		}
		if body.read > maxDrainBytes {
			t.Errorf("read %d bytes of the body, want at most %d", body.read, maxDrainBytes)
		}
		if strings.Contains(err.Error(), "xxxx") {
			t.Errorf("error text quotes the body: %v", err)
		}
	})
}
