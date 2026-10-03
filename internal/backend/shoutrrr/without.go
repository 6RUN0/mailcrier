//go:build noshoutrrr

package shoutrrr

import (
	"context"
	"errors"
	"net/http"

	"github.com/6RUN0/mailcrier/internal/backend"
)

// Options configure one shoutrrr target.
type Options struct {
	// URL is the service URL.
	URL string
	// Client is unused in this build.
	Client *http.Client
}

// Sender is never created in this build.
type Sender struct{}

// New rejects every target: the binary was built without the library.
func New(Options) (*Sender, error) {
	return nil, errors.New("built without shoutrrr")
}

// Caps reports no limits.
func (*Sender) Caps() backend.Caps {
	return backend.Caps{}
}

// Send fails for good.
func (*Sender) Send(context.Context, backend.Payload) error {
	return &backend.Error{Class: backend.Permanent, Err: errors.New("built without shoutrrr")}
}
