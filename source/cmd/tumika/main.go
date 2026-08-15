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

	// One channel for both signals rather than signal.NotifyContext plus a
	// second registration.
	//
	// The two-channel version had a race: the second channel was only registered
	// after the context was already cancelled, and the signal package delivers to
	// whichever channels are registered at the moment a signal arrives. A SIGINT
	// landing inside that window went to NotifyContext's channel — buffered 1 and
	// already full — and was dropped, so an operator double-tapping Ctrl-C on a
	// wedged shutdown saw the second press do nothing.
	//
	// Registering once up front and reading twice has no such window: the buffer
	// holds the second signal until we get to it.
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		<-signals
		// First signal: unwind. Long-running commands stop their runners, drain
		// in-flight requests and close the database.
		cancel()

		<-signals
		// Second signal: the operator is not willing to wait. Stop handling
		// signals first, so a third takes the default disposition if this exit
		// somehow blocks.
		signal.Stop(signals)
		os.Exit(1)
	}()

	os.Exit(cli.Execute(ctx))
}
