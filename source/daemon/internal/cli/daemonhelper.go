package cli

import (
	"github.com/spf13/cobra"

	"github.com/tumika/tumika/source/daemon/internal/daemon"
)

// withDaemon opens the daemon's resources, runs fn, and closes them.
//
// A command routes through here only when it must work without a serving
// daemon (ADR-0013): `token` on a fresh install, `update` on a daemon that
// will not stay up, and `config set`/`reset` on a setting that is the reason
// it will not serve. Every other command stays an HTTP client of the running
// daemon, because the CLI is not a second entry point into the services
// (ADR-0004).
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
		// A CLI command is not a boot of the daemon. Only `serve` resolves the
		// previous update, so an ordinary command can neither count a boot
		// attempt against a pending update nor roll one back underneath itself.
		SkipUpdateBoot: true,
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
