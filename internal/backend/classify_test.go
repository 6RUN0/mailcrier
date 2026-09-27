package backend

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"testing"
	"time"
)

func TestClassify(t *testing.T) {
	timeout := &url.Error{Op: "Post", URL: "http://example.org", Err: context.DeadlineExceeded}
	refused := &url.Error{Op: "Post", URL: "http://example.org", Err: &net.OpError{Op: "dial", Err: errors.New("connection refused")}}

	cases := []struct {
		name   string
		status int
		err    error
		want   Class
	}{
		{"rate-limited", http.StatusTooManyRequests, nil, Temporary},
		{"internal-server-error", http.StatusInternalServerError, nil, Temporary},
		{"bad-gateway", http.StatusBadGateway, nil, Temporary},
		{"service-unavailable", http.StatusServiceUnavailable, nil, Temporary},
		{"gateway-timeout", http.StatusGatewayTimeout, nil, Temporary},
		{"timeout", 0, timeout, Temporary},
		{"network-error", 0, refused, Temporary},
		{"bad-request", http.StatusBadRequest, nil, Permanent},
		{"unauthorized", http.StatusUnauthorized, nil, Permanent},
		{"forbidden", http.StatusForbidden, nil, Permanent},
		{"not-found", http.StatusNotFound, nil, Permanent},
		{"payload-too-large", http.StatusRequestEntityTooLarge, nil, Permanent},
		{"unexpected-redirect", http.StatusFound, nil, Permanent},
	}
	for _, tc := range cases {
		t.Run("T-ADJ-21/"+tc.name, func(t *testing.T) {
			if got := Classify(tc.status, http.Header{}, tc.err); got != tc.want {
				t.Errorf("Classify(%d, %v) = %v, want %v", tc.status, tc.err, got, tc.want)
			}
		})
	}
}

// TestClassifyAnyServerError pins that every 5xx answer to a POST is
// temporary, those a retry rarely cures included: a wrong retry costs a
// duplicate, a wrong give-up the message.
func TestClassifyAnyServerError(t *testing.T) {
	check := func(status int) func(t *testing.T) {
		return func(t *testing.T) {
			if got := Classify(status, http.Header{}, nil); got != Temporary {
				t.Errorf("Classify(%d) = %v, want temporary", status, got)
			}
		}
	}
	t.Run("T-ADJ-46/not-implemented", check(http.StatusNotImplemented))
	t.Run("T-ADJ-46/http-version-not-supported", check(http.StatusHTTPVersionNotSupported))
	t.Run("T-ADJ-46/insufficient-storage", check(http.StatusInsufficientStorage))
	t.Run("T-ADJ-46/network-authentication-required", check(http.StatusNetworkAuthenticationRequired))
}

// TestClassifyRetryAfter pins that a Retry-After header makes any answer
// temporary: some services send it with 403 for a rate limit.
func TestClassifyRetryAfter(t *testing.T) {
	header := http.Header{"Retry-After": {"30"}}
	for _, status := range []int{http.StatusForbidden, http.StatusBadRequest, http.StatusTooManyRequests} {
		if got := Classify(status, header, nil); got != Temporary {
			t.Errorf("Classify(%d, Retry-After: 30) = %v, want temporary", status, got)
		}
	}
}

func TestRetryAfter(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  time.Duration
	}{
		{"seconds", "17", 17 * time.Second},
		{"zero", "0", 0},
		{"absent", "", 0},
		{"http-date", "Wed, 21 Oct 2015 07:28:00 GMT", 0},
		{"negative", "-5", 0},
		{"bounded", "999999999", MaxRetryAfter},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			header := http.Header{}
			if tc.value != "" {
				header.Set("Retry-After", tc.value)
			}
			if got := RetryAfter(header); got != tc.want {
				t.Errorf("RetryAfter(%q) = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}

func TestErrorKeepsCause(t *testing.T) {
	cause := errors.New("boom")
	err := error(&Error{Class: Permanent, Status: http.StatusBadRequest, Err: cause})
	if !errors.Is(err, cause) {
		t.Fatalf("errors.Is(%v, cause) = false", err)
	}
	if got, want := err.Error(), "permanent failure, status 400: boom"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	noStatus := &Error{Class: Temporary, Err: cause}
	if got, want := noStatus.Error(), "temporary failure: boom"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}
