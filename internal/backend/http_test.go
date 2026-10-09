package backend

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// roundTripFunc answers a request without a network.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// trackedBody records whether Do closed it.
type trackedBody struct {
	io.Reader
	isClosed bool
}

func (b *trackedBody) Close() error {
	b.isClosed = true
	return nil
}

// answering returns a client whose every request gets status, header and
// body.
func answering(status int, header http.Header, body io.ReadCloser) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: status, Header: header, Body: body}, nil
	})}
}

func newRequest(t *testing.T) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "http://example.org/SECRET", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func TestDoStatus(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		wantErr    bool
		wantClass  Class
		wantStatus int
	}{
		{"ok", http.StatusOK, false, 0, 0},
		{"no-content", http.StatusNoContent, false, 0, 0},
		{"server-error", http.StatusBadGateway, true, Temporary, http.StatusBadGateway},
		{"rate-limited", http.StatusTooManyRequests, true, Temporary, http.StatusTooManyRequests},
		{"rejected", http.StatusBadRequest, true, Permanent, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := &trackedBody{Reader: strings.NewReader("echo of the request")}
			err := Do(answering(tc.status, http.Header{}, body), newRequest(t), nil)
			if !body.isClosed {
				t.Error("Do() left the body open")
			}
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("Do() error = %v", err)
				}
				return
			}
			var deliveryErr *Error
			if !errors.As(err, &deliveryErr) {
				t.Fatalf("Do() error = %v, want *Error", err)
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

func TestDoRetryAfter(t *testing.T) {
	client := answering(http.StatusServiceUnavailable, http.Header{"Retry-After": {"120"}}, http.NoBody)
	var deliveryErr *Error
	if err := Do(client, newRequest(t), nil); !errors.As(err, &deliveryErr) || deliveryErr.Class != Temporary || deliveryErr.RetryAfter != 2*time.Minute {
		t.Errorf("Do() error = %+v, want temporary with RetryAfter 2m", err)
	}
}

// TestDoParseReadsBody pins that parse decides for any status and reads
// the body before Do drains it.
func TestDoParseReadsBody(t *testing.T) {
	body := &trackedBody{Reader: strings.NewReader("answer")}
	parseErr := errors.New("parsed")
	err := Do(answering(http.StatusOK, http.Header{}, body), newRequest(t), func(resp *http.Response) error {
		if body.isClosed {
			t.Error("body closed before parse")
		}
		got, err := io.ReadAll(resp.Body)
		if err != nil || string(got) != "answer" {
			t.Errorf("parse read %q, %v", got, err)
		}
		return parseErr
	})
	if !errors.Is(err, parseErr) {
		t.Errorf("Do() error = %v, want the error of parse", err)
	}
	if !body.isClosed {
		t.Error("Do() left the body open")
	}
}

// TestDoParseDecidesOverStatus pins that Do leaves a failed status to
// parse instead of checking it first.
func TestDoParseDecidesOverStatus(t *testing.T) {
	client := answering(http.StatusInternalServerError, http.Header{}, http.NoBody)
	if err := Do(client, newRequest(t), func(*http.Response) error { return nil }); err != nil {
		t.Errorf("Do() error = %v, want the nil of parse", err)
	}
}

func TestDoTransportErrorWithoutURL(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("connection refused")
	})}
	err := Do(client, newRequest(t), func(*http.Response) error {
		t.Error("parse called without a response")
		return nil
	})
	var deliveryErr *Error
	if !errors.As(err, &deliveryErr) || deliveryErr.Class != Temporary {
		t.Fatalf("Do() error = %+v, want temporary *Error", err)
	}
	if strings.Contains(err.Error(), "SECRET") {
		t.Errorf("error text contains the URL: %v", err)
	}
}

// TestStatusCause pins the description of a redirect: the scheme and
// host of its Location alone, the host alone when the Location has no
// scheme, and no path in either.
func TestStatusCause(t *testing.T) {
	for _, tc := range []struct {
		location, want string
	}{
		{"https://ntfy.example.org/alerts?x=1", "redirect to https://ntfy.example.org not followed"},
		{"//cdn.example.org/alerts", "redirect to cdn.example.org not followed"},
		{"/alerts/", "redirect to another path not followed"},
		{"", "redirect not followed"},
	} {
		header := http.Header{}
		if tc.location != "" {
			header.Set("Location", tc.location)
		}
		if got := StatusCause(http.StatusFound, header).Error(); got != tc.want {
			t.Errorf("StatusCause(302, Location %q) = %q, want %q", tc.location, got, tc.want)
		}
	}
}
