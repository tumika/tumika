package cli

import (
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
			if err != nil {
				return err
			}
			defer func() {
				if err := d.Close(); err != nil {
					g.logger.Error("closing the database", "err", err)
				}
			}()

			return d.Serve(ctx)
		},
	}

	cmd.Flags().StringVar(&listen, "listen", "",
		"address to listen on, overriding the server.listen setting")

	return cmd
}
