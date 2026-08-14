// Command tumika is the daemon and its command-line interface.
//
// This file stays thin on purpose: the build-time variables and the process
// lifecycle, and nothing else. Everything the CLI does lives in
// source/internal/cli.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/tumika/tumika/source/internal/cli"
	"github.com/tumika/tumika/source/internal/platform/buildinfo"
)

// Injected at release time via -ldflags "-X main.version=… -X main.commit=… -X main.date=…".
//
// Keep these names and this package stable: goreleaser writes them, and the
// self-updater short-circuits on the "dev" default (AGENTS.md, "Version
// injection").
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	buildinfo.Set(version, commit, date)

	// The first signal cancels the context, which is how every long-running
	// command unwinds: the daemon stops its runners, drains in-flight requests
	// and closes the database.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// A second signal means the operator is not willing to wait for that. Give
	// them an immediate exit rather than a process that appears wedged —
	// stopping the notifier first so the signal reverts to its default
	// disposition if a third arrives.
	go func() {
		<-ctx.Done()
		second := make(chan os.Signal, 1)
		signal.Notify(second, os.Interrupt, syscall.SIGTERM)
		<-second
		stop()
		os.Exit(1)
	}()

	os.Exit(cli.Execute(ctx))
}
