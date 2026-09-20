// Command tumika is the daemon and its command-line interface.
//
// This file stays thin on purpose: the build-time variables and the process
// lifecycle, and nothing else. Everything the CLI does lives in
// source/daemon/internal/cli.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/tumika/tumika/source/daemon/internal/cli"
	"github.com/tumika/tumika/source/daemon/internal/platform/buildinfo"
	"github.com/tumika/tumika/source/daemon/internal/repository/migrations"
)

// Injected at release time via -ldflags "-X main.version=… -X main.release=…
// -X main.commit=… -X main.date=…".
//
// version is this component's semver; release is the label of the release that
// shipped it. Keep these names and this package stable: goreleaser writes them,
// and the self-updater short-circuits on the "dev" default (agentic/tumika-repo.md,
// "Version injection").
var (
	version = "dev"
	release = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	// The embedded migrations are read here because depguard keeps the cli and
	// platform layers out of the repository package: main is the one place that
	// can learn the number and hand it to buildinfo. An error means the binary
	// was built without its migrations, so it could never migrate a database
	// either — saying so before any command runs is the earliest honest failure.
	schemaVersion, err := migrations.MaxVersion()
	if err != nil {
		fmt.Fprintf(os.Stderr, "tumika: %v\n", err)
		os.Exit(1)
	}
	buildinfo.Set(version, release, commit, date, schemaVersion)

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
