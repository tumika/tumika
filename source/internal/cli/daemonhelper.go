package cli

import (
	"github.com/spf13/cobra"

	"github.com/tumika/tumika/source/internal/daemon"
)

// withDaemon opens the daemon's resources, runs fn, and closes them.
//
// For commands that must work before a daemon is running — `token` on a fresh
// install being the case that matters, since the daemon refuses to serve without
// one. Everything else talks to the running daemon over HTTP, because the CLI is
// an API client and not a second entry point into the services (ADR-0004).
func withDaemon(g *globals, cmd *cobra.Command, fn func(*daemon.Daemon) error) error {
	p, err := g.Paths()
	if err != nil {
		return err
	}

	d, err := daemon.New(cmd.Context(), daemon.Options{
		Paths:  p,
		Logger: g.logger,
		// Nil unless Execute supplied one, and nil means store nothing — so
		// `token rotate` and `install` reach the platform secret store from a
		// real invocation and from nowhere else.
		TokenCustody: g.tokenCustody,
	})
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := d.Close(); closeErr != nil {
			g.logger.Error("closing the database", "err", closeErr)
		}
	}()

	return fn(d)
}
