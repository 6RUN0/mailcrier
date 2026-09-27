package backend

import "net/http"

// Classify maps the outcome of an HTTP exchange to a Class. err is the error
// returned by the HTTP client, status the response code when a response
// arrived.
//
// Every transport error counts as Temporary: a timeout or a refused
// connection says nothing about whether the next attempt fails too, and a
// wrong retry costs one duplicate at most, while a wrong give-up loses the
// message. Rate limiting and server errors are Temporary for the same
// reason; any other status means the service rejected this very request.
func Classify(status int, err error) Class {
	if err != nil {
		return Temporary
	}
	if status == http.StatusTooManyRequests || status >= http.StatusInternalServerError {
		return Temporary
	}
	return Permanent
}
