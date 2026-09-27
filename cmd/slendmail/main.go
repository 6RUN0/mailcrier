// Command slendmail is a sendmail replacement that delivers the mail of
// cron, at and other system tools to chat and webhook services.
package main

import (
	"context"
	"os"

	"github.com/6RUN0/slendmail/internal/app"
)

func main() {
	os.Exit(app.Run(context.Background(), app.SystemDeps(), os.Args[1:], os.Stdin))
}
