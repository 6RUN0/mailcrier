package app

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

// CancelOnSignal returns a context that SIGINT, SIGTERM or SIGHUP cancels,
// and the function that stops listening. A signal the process was started
// with ignored, as nohup and a background command of sh do, stays ignored:
// listening for it would undo that. A cancelled call ends what it
// waits for as a temporary failure kept in the spool: an HTTP request
// returns, and a hook, which runs in a process group of its own and so
// misses a Ctrl-C of the terminal, is killed with its group. After the
// first signal the default action applies again, so a second one ends the
// process at once.
func CancelOnSignal(parent context.Context) (context.Context, context.CancelFunc) {
	var caught []os.Signal
	for _, sig := range []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP} {
		if !signal.Ignored(sig) {
			caught = append(caught, sig)
		}
	}
	if len(caught) == 0 {
		return context.WithCancel(parent)
	}
	ctx, stop := signal.NotifyContext(parent, caught...)
	go func() {
		<-ctx.Done()
		stop()
	}()
	return ctx, stop
}
