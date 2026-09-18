// Command agentd is the Agentd runner: a single static binary that executes
// scheduled checks over external sources.
//
// This file is the composition root. It is the only place that knows about
// both the core and every adapter: it constructs adapters, injects them into
// the core through ports, and wires signal handling. No domain rules live
// here.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "agentd: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	// Wiring goes here once the core has something to wire:
	//
	//	clk   := clock.System{}
	//	st, err := store.Open(cfg.DatabasePath)   // SQLite, WAL
	//	sched := scheduling.New(clk, rng, st, ...)
	//	return cli.Execute(ctx, sched, ...)
	_ = ctx
	return fmt.Errorf("not implemented: core wiring pending the architecture specification")
}
