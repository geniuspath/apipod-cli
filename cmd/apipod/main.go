// Command apipod is the APIPod command-line client.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/geniuspath/apipod-cli/internal/cli"
)

// Set by the release build (-ldflags -X).
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	// After the first signal, restore the default handling: a second Ctrl-C
	// ends the process even if something is not watching the context.
	go func() {
		<-ctx.Done()
		stop()
	}()
	app := cli.NewApp(cli.BuildInfo{Version: version, Commit: commit, Date: date})
	code := app.Run(ctx, os.Args[1:])
	stop()
	os.Exit(code)
}
