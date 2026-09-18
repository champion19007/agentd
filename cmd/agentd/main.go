// Command agentd is the Agentd runner: a single static binary that executes
// scheduled checks over external sources.
//
// This file is the composition root. It is the only place that knows about
// both the core and every adapter, and its whole job is to construct adapters
// and inject them through ports. No domain rule lives here.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/champion19007/agentd/internal/cli"
)

func main() {
	// A signalled shutdown cancels the context, which travels through every
	// port: an in-flight fetch is abandoned, its run is recorded as
	// interrupted rather than failed, and the database closes cleanly.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	os.Exit(cli.Main(ctx, cli.DefaultEnv(), os.Args[1:]))
}
