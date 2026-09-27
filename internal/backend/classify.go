package backend

import (
	"net/http"
	"strconv"
	"time"
)

// Classify maps the outcome of an HTTP exchange to a Class. err is the error
// returned by the HTTP client, status and header those of the response when
// one arrived.
//
// Every transport error counts as Temporary: a timeout or a refused
// connection says nothing about whether the next attempt fails too, and a
// wrong retry costs one duplicate at most, while a wrong give-up loses the
// message. Rate limiting and server errors are Temporary for the same
// reason, and so is any answer with a Retry-After header, which some
// services send with 403 for a rate limit; any other status means the
// service rejected this very request.
func Classify(status int, header http.Header, err error) Class {
	if err != nil {
		return Temporary
	}
	if status == http.StatusTooManyRequests || status >= http.StatusInternalServerError || header.Get("Retry-After") != "" {
		return Temporary
	}
	return Permanent
}

// MaxRetryAfter bounds a delay a service asks for, so that a bogus
// value cannot park a message for years.
const MaxRetryAfter = 24 * time.Hour

// RetryAfter returns the delay of a Retry-After header in whole seconds,
// at most MaxRetryAfter; 0 when the header is absent or holds an HTTP
// date, which the chat services do not send.
func RetryAfter(header http.Header) time.Duration {
	seconds, err := strconv.ParseUint(header.Get("Retry-After"), 10, 32)
	if err != nil {
		return 0
	}
	return min(time.Duration(seconds)*time.Second, MaxRetryAfter)
}
