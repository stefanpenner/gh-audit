package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/stefanpenner/gh-audit/cmd"
)

func main() {
	// SIGINT/SIGTERM cancel the command context so in-flight work unwinds
	// cooperatively (DB writes finish or roll back instead of being killed
	// between a delete and its re-insert). A second signal falls back to
	// the default hard-kill behaviour because NotifyContext unregisters
	// after the first one.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := cmd.NewRootCmd().ExecuteContext(ctx); err != nil {
		os.Exit(1)
	}
}
