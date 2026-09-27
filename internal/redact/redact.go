// Package redact replaces registered secrets with a mask in everything the
// process logs. It is a leaf of the package graph: the caller registers the
// secrets it loads and wraps its log destinations.
package redact

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"slices"
	"strings"
	"sync"
)

// Mask replaces every occurrence of a secret.
const Mask = "***"

// minFragmentLength is the shortest URL path segment or query value
// registered on its own. Tokens embedded in webhook URLs are 24 characters
// and longer; shorter segments such as "hooks" or "services" are not
// secret, and masking them would garble unrelated log text.
const minFragmentLength = 16

// Redactor holds the secrets to mask. The zero value masks nothing and is
// ready to use; it is safe for concurrent use.
type Redactor struct {
	mu       sync.RWMutex
	secrets  []string
	replacer *strings.Replacer
}

// Add registers secret. An empty string is ignored.
func (r *Redactor) Add(secret string) {
	if secret == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if slices.Contains(r.secrets, secret) {
		return
	}
	r.secrets = append(r.secrets, secret)
	// Longest first, so that a secret containing another is masked whole
	// instead of leaving its remainder in the text.
	slices.SortFunc(r.secrets, func(a, b string) int { return len(b) - len(a) })
	pairs := make([]string, 0, 2*len(r.secrets))
	for _, s := range r.secrets {
		pairs = append(pairs, s, Mask)
	}
	r.replacer = strings.NewReplacer(pairs...)
}

// AddURL registers a URL that may embed a secret, together with the parts
// of it that texts quote on their own, as they appear in the escaped URL:
//   - the host with and without port, and each of its labels, when at least
//     minFragmentLength long (a DNS or certificate error names the host,
//     and some services put the secret in a subdomain);
//   - the userinfo with its password, and a long user name (shoutrrr URLs
//     carry tokens there);
//   - the request URI when it is long or has a query (an HTTP/2 debug log
//     prints only the ":path");
//   - every path segment and query value of at least minFragmentLength.
func (r *Redactor) AddURL(raw string) {
	r.Add(raw)
	parsed, err := url.Parse(raw)
	if err != nil {
		return
	}
	for _, host := range []string{parsed.Host, parsed.Hostname()} {
		if len(host) >= minFragmentLength {
			r.Add(host)
		}
	}
	for _, label := range strings.Split(parsed.Hostname(), ".") {
		if len(label) >= minFragmentLength {
			r.Add(label)
		}
	}
	if uri := parsed.RequestURI(); len(uri) >= minFragmentLength || parsed.RawQuery != "" {
		r.Add(uri)
	}
	if password, ok := parsed.User.Password(); ok {
		r.Add(parsed.User.String())
		r.Add(password)
	}
	if username := parsed.User.Username(); len(username) >= minFragmentLength {
		r.Add(username)
	}
	for _, segment := range strings.Split(parsed.EscapedPath(), "/") {
		if len(segment) >= minFragmentLength {
			r.Add(segment)
		}
	}
	for _, pair := range strings.Split(parsed.RawQuery, "&") {
		if _, value, _ := strings.Cut(pair, "="); len(value) >= minFragmentLength {
			r.Add(value)
		}
	}
}

// String returns s with every registered secret replaced by Mask.
func (r *Redactor) String(s string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.replacer == nil {
		return s
	}
	return r.replacer.Replace(s)
}

// Writer returns a writer that masks secrets in each Write before passing
// it to w. A secret split across two Write calls is not recognized; the
// log package and slog handlers write one whole record per call.
func (r *Redactor) Writer(w io.Writer) io.Writer {
	return &writer{redactor: r, dst: w}
}

type writer struct {
	redactor *Redactor
	dst      io.Writer
}

// Write masks p and reports the whole of p as written.
func (w *writer) Write(p []byte) (int, error) {
	if _, err := io.WriteString(w.dst, w.redactor.String(string(p))); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Handler returns a slog.Handler that masks secrets in the message and in
// every attribute before passing the record to next. Values of kind Any,
// errors among them, are rendered with fmt.Sprint and masked as text.
// Attributes attached with With are masked when a record is handled, so a
// secret registered after the logger was built is still masked.
func (r *Redactor) Handler(next slog.Handler) slog.Handler {
	return &handler{redactor: r, next: next}
}

type handler struct {
	redactor *Redactor
	next     slog.Handler
	// steps are the WithAttrs and WithGroup calls made on this handler, in
	// order, replayed on next for every record.
	steps []step
}

// step is one WithAttrs (attrs set) or WithGroup (group set) call.
type step struct {
	attrs []slog.Attr
	group string
}

// Enabled defers to the wrapped handler.
func (h *handler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

// Handle masks the record and the attached attributes and passes them on.
func (h *handler) Handle(ctx context.Context, record slog.Record) error {
	next := h.next
	for _, s := range h.steps {
		if s.attrs != nil {
			next = next.WithAttrs(h.redactor.attrs(s.attrs))
		} else {
			next = next.WithGroup(s.group)
		}
	}
	masked := slog.NewRecord(record.Time, record.Level, h.redactor.String(record.Message), record.PC)
	record.Attrs(func(attr slog.Attr) bool {
		masked.AddAttrs(h.redactor.attr(attr))
		return true
	})
	return next.Handle(ctx, masked)
}

// WithAttrs records attrs for masking at Handle time.
func (h *handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	return h.with(step{attrs: slices.Clone(attrs)})
}

// WithGroup records the group for Handle time.
func (h *handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return h.with(step{group: name})
}

func (h *handler) with(s step) *handler {
	return &handler{redactor: h.redactor, next: h.next, steps: append(slices.Clip(h.steps), s)}
}

func (r *Redactor) attrs(attrs []slog.Attr) []slog.Attr {
	masked := make([]slog.Attr, len(attrs))
	for i, attr := range attrs {
		masked[i] = r.attr(attr)
	}
	return masked
}

func (r *Redactor) attr(attr slog.Attr) slog.Attr {
	value := attr.Value.Resolve()
	switch value.Kind() {
	case slog.KindString:
		return slog.String(attr.Key, r.String(value.String()))
	case slog.KindAny:
		return slog.String(attr.Key, r.String(fmt.Sprint(value.Any())))
	case slog.KindGroup:
		return slog.Attr{Key: attr.Key, Value: slog.GroupValue(r.attrs(value.Group())...)}
	default:
		return slog.Attr{Key: attr.Key, Value: value}
	}
}
