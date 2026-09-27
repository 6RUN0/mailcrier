// Command slendmail is a sendmail replacement that delivers the mail of
// cron, at and other system tools to chat and webhook services.
package main

import (
	"context"
	"os"
	"syscall"

	"github.com/6RUN0/slendmail/internal/app"
)

// umask keeps every file the process creates, with the group of a setgid
// binary, out of reach of other users.
const umask = 0o007

// main hardens the process before anything else: a setgid process must not
// read the caller's environment, not even TZ, before it is sanitized.
func main() {
	syscall.Umask(umask)
	args, reexecErr := app.Harden(app.CurrentProcess())
	deps := app.SystemDeps()
	deps.ReexecErr = reexecErr
	os.Exit(app.Run(context.Background(), deps, args, os.Stdin))
}
