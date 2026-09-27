package backend

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"testing"
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
			if got := Classify(tc.status, tc.err); got != tc.want {
				t.Errorf("Classify(%d, %v) = %v, want %v", tc.status, tc.err, got, tc.want)
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
