//go:build !noshoutrrr

package shoutrrr

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"runtime/debug"
	"slices"
	"strings"
	"sync"

	"github.com/nicholas-fedor/shoutrrr/pkg/router"
	"github.com/nicholas-fedor/shoutrrr/pkg/types"

	"github.com/6RUN0/mailcrier/internal/backend"
)

// Options configure one shoutrrr target.
type Options struct {
	// URL is the service URL; it may embed a secret.
	URL string
	// Client performs the requests of the services that accept a client;
	// it must not be nil. New uses a copy that does not follow redirects.
	Client *http.Client
}

// Sender sends the text through one shoutrrr service.
type Sender struct {
	service  types.Service
	recorder *responseRecorder
	// busy holds a token from the start of a send until the library
	// returns, which may be after Send gave up at the deadline: the
	// recorder belongs to one send at a time, and a service is never
	// called from two goroutines.
	busy chan struct{}
}

// outcome is what a send of the library left: its error and the last
// answer the recorder saw, read before the next send may start.
type outcome struct {
	err          error
	status       int
	header       http.Header
	transportErr *backend.Error
	// panicErr is a panic of the library, nil without one.
	panicErr *backend.PanicError
}

// New locates the service of opts.URL, so that an unknown scheme or a
// URL the service rejects fails with the configuration. The error does
// not quote the URL.
func New(opts Options) (*Sender, error) {
	recorder := &responseRecorder{client: backend.WithoutRedirects(opts.Client)}
	serviceRouter, err := router.NewWithOptions(nil, types.SenderOptions{HTTPClient: recorder})
	if err != nil {
		return nil, errors.New("shoutrrr router not created")
	}
	scheme := ""
	if parsed, parseErr := url.Parse(opts.URL); parseErr == nil {
		scheme = parsed.Scheme
		// The matrix service logs in when it is located, with a client of
		// its own and outside the deadline: a server that is down would
		// reject the whole configuration. An access token needs no login.
		name, _, _ := strings.Cut(strings.ToLower(scheme), "+")
		if name == "matrix" && parsed.User.Username() != "" {
			return nil, errors.New("matrix login by password is not supported, use an access token")
		}
	}
	service, err := serviceRouter.Locate(opts.URL)
	switch {
	case errors.Is(err, router.ErrUnknownService):
		return nil, fmt.Errorf("unknown shoutrrr service %q", scheme)
	case err != nil:
		return nil, fmt.Errorf("URL rejected by shoutrrr service %q: %s", scheme, withoutURLParts(err, opts.URL))
	}
	return &Sender{service: service, recorder: recorder, busy: make(chan struct{}, 1)}, nil
}

// minURLPart is the shortest part of the URL that withoutURLParts masks:
// a shorter one is a word such as "id", whose masking would garble the
// text.
const minURLPart = 3

// withoutURLParts returns the text of an error of the library with the
// parts of raw that may hold a secret replaced by ***: user information,
// host and its labels, path segments, raw and decoded, and query values.
// The library quotes the token it rejects whatever its length, several
// services keep it in the host (pushover://shoutrrr:token@userKey/), and
// the redactor of the log masks a short part only within the whole URL.
// The scheme stays, and so does a host that only repeats the name of the
// service (telegram://token@telegram): they name the service.
func withoutURLParts(err error, raw string) string {
	text := backend.WithoutURL(err).Error()
	parsed, parseErr := url.Parse(raw)
	if parseErr != nil {
		return "invalid URL"
	}
	parts := []string{raw, parsed.Host, parsed.Hostname()}
	parts = append(parts, strings.Split(parsed.Hostname(), ".")...)
	parts = append(parts, strings.Split(parsed.Path, "/")...)
	parts = append(parts, strings.Split(parsed.EscapedPath(), "/")...)
	for _, values := range parsed.Query() {
		parts = append(parts, values...)
	}
	if user := parsed.User; user != nil {
		parts = append(parts, user.String(), user.Username())
		if password, ok := user.Password(); ok {
			parts = append(parts, password)
		}
	}
	// The longest first, so that a part inside another one does not leave
	// the rest of it.
	slices.SortFunc(parts, func(a, b string) int { return len(b) - len(a) })
	service, _, _ := strings.Cut(strings.ToLower(parsed.Scheme), "+")
	for _, part := range parts {
		if len(part) >= minURLPart && strings.ToLower(part) != service {
			text = strings.ReplaceAll(text, part, "***")
		}
	}
	return text
}

// Caps reports neither a text limit nor files: each service cuts or
// splits a long text itself, and the target sends text only.
func (s *Sender) Caps() backend.Caps {
	return backend.Caps{}
}

// Send sends p.Text and classifies the outcome. A failure after an HTTP
// answer outside 2xx is classified by that status, as for the native
// targets; any other failure is temporary, because shoutrrr does not tell
// a rejected request from a network error, and the spool bounds the
// retries by its TTL. A redirect counts as a permanent failure even when
// the service reports success: some services accept any status below
// 400. A panic of the library is a permanent failure that wraps a
// *backend.PanicError.
func (s *Sender) Send(ctx context.Context, p backend.Payload) error {
	select {
	case s.busy <- struct{}{}:
	case <-ctx.Done():
		return &backend.Error{Class: backend.Temporary, Err: fmt.Errorf("previous shoutrrr send still running: %w", context.Cause(ctx))}
	}
	s.recorder.reset()
	done := make(chan outcome, 1)
	go func() {
		defer func() { <-s.busy }()
		// No recover of delivery reaches this goroutine: a panic of the
		// library would end the process with its value, which may quote
		// the URL, on stderr.
		defer func() {
			if value := recover(); value != nil {
				done <- outcome{panicErr: &backend.PanicError{Value: value, Stack: debug.Stack()}}
			}
		}()
		var err error
		if sender, ok := s.service.(types.ContextSender); ok {
			err = sender.SendContext(ctx, p.Text, &types.Params{})
		} else {
			err = s.service.Send(p.Text, &types.Params{})
		}
		status, header, transportErr := s.recorder.last()
		done <- outcome{err: err, status: status, header: header, transportErr: transportErr}
	}()
	var result outcome
	select {
	case result = <-done:
	case <-ctx.Done():
		// A service without context support goes on until the request
		// timeout of the client; its result is dropped.
		return &backend.Error{Class: backend.Temporary, Err: fmt.Errorf("shoutrrr send: %w", context.Cause(ctx))}
	}
	err, status, header, transportErr := result.err, result.status, result.header, result.transportErr
	if result.panicErr != nil {
		return &backend.Error{Class: backend.Permanent, Err: result.panicErr}
	}
	switch {
	case err == nil && status >= 300 && status < 400:
		return &backend.Error{Class: backend.Permanent, Status: status, Err: backend.StatusCause(status, header)}
	case err == nil:
		return nil
	case transportErr != nil:
		return transportErr
	case status >= 300:
		return &backend.Error{
			Class: backend.Classify(status, header, nil), Status: status, RetryAfter: backend.RetryAfter(header),
			Err: backend.StatusCause(status, header),
		}
	default:
		// The text of shoutrrr may quote the URL; the redactor masks it
		// in the log and in the spool.
		return &backend.Error{Class: backend.Temporary, Err: backend.WithoutURL(err)}
	}
}

// responseRecorder is the HTTP client of the services; it remembers the
// outcome of the last request, status and header or transport error,
// which shoutrrr reports only as text, with the URL in it. The transport
// error is kept as backend.RequestError makes it.
type responseRecorder struct {
	client       *http.Client
	mu           sync.Mutex
	status       int
	header       http.Header
	transportErr *backend.Error
}

func (r *responseRecorder) Do(req *http.Request) (*http.Response, error) {
	resp, err := r.client.Do(req)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status, r.header, r.transportErr = 0, nil, nil
	if err != nil {
		r.transportErr = backend.RequestError(r.client, req, err)
	} else {
		r.status, r.header = resp.StatusCode, resp.Header.Clone()
	}
	return resp, err
}

func (r *responseRecorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status, r.header, r.transportErr = 0, nil, nil
}

func (r *responseRecorder) last() (int, http.Header, *backend.Error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status, r.header, r.transportErr
}
