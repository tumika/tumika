package cli

import (
	"errors"

	"github.com/spf13/cobra"

	"github.com/tumika/tumika/source/internal/daemon"
)

func newServeCmd(g *globals) *cobra.Command {
	var listen string

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the daemon",
		Long: "Run the tumika daemon in the foreground.\n\n" +
			"Under a service manager this is the ExecStart. Run directly, it stops on the\n" +
			"first SIGINT or SIGTERM, draining in-flight requests; a second signal exits\n" +
			"immediately.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, err := g.Paths()
			if err != nil {
				return err
			}

			ctx := cmd.Context()
			d, err := daemon.New(ctx, daemon.Options{
				Paths:  p,
				Logger: g.logger,
				Listen: listen,
			})
			// A rollback now happens during CONSTRUCTION, before migrations, so
			// New can ask for a restart just as Serve can. Exiting zero is what
			// makes the supervisor relaunch onto the restored binary.
			if errors.Is(err, daemon.ErrRestartRequired) {
				return nil
			}
			if err != nil {
				return err
			}
			defer func() {
				if err := d.Close(); err != nil {
					g.logger.Error("closing the database", "err", err)
				}
			}()

			// A restart request is a SUCCESS, not a failure.
			//
			// The daemon returns it after installing an update, or after
			// rolling one back. Exiting zero is what makes systemd's
			// Restart=always and launchd's KeepAlive relaunch it — reporting
			// an error would work too, but it would put a spurious failure in
			// the journal on every successful update and teach an operator to
			// ignore the word.
			err = d.Serve(ctx)
			if errors.Is(err, daemon.ErrRestartRequired) {
				return nil
			}
			return err
		},
	}

	cmd.Flags().StringVar(&listen, "listen", "",
		"address to listen on, overriding the server.listen setting")

	return cmd
}
