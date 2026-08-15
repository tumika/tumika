package cli

import (
	"github.com/spf13/cobra"

	"github.com/tumika/tumika/source/internal/daemon"
)

func newTokenCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "token",
		Short: "Manage the API bearer token",
		Long: "Show or rotate the token the HTTP API requires.\n\n" +
			"Only the token's SHA-256 is stored, so an existing token cannot be shown —\n" +
			"a lost token is replaced, not recovered.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withDaemon(g, cmd, func(d *daemon.Daemon) error {
				configured, err := d.AuthService().Configured(cmd.Context())
				if err != nil {
					return err
				}
				if configured {
					printf(cmd, "An API token is configured.\n")
					printf(cmd, "Run `tumika token rotate` to replace it; it cannot be shown.\n")
					return nil
				}
				printf(cmd, "No API token is configured. The daemon will refuse to serve.\n")
				printf(cmd, "Run `tumika token rotate` to create one.\n")
				return nil
			})
		},
	}

	cmd.AddCommand(newTokenRotateCmd(g))
	return cmd
}

func newTokenRotateCmd(g *globals) *cobra.Command {
	var quiet bool

	cmd := &cobra.Command{
		Use:   "rotate",
		Short: "Mint a new API token, replacing any existing one",
		Long: "Mint a new API token and print it once.\n\n" +
			"Only its SHA-256 is stored, so this is the only time the token exists in a\n" +
			"form anyone can read. Any client using the previous token stops working\n" +
			"immediately.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withDaemon(g, cmd, func(d *daemon.Daemon) error {
				token, err := d.AuthService().Rotate(cmd.Context())
				if err != nil {
					return err
				}

				// The token goes to stdout and nowhere else. Not through the
				// logger — which would put a live credential into the journal,
				// exactly what the redaction rules exist to prevent — and not
				// into a file, so the only copy is the one the operator keeps.
				if quiet {
					printf(cmd, "%s\n", token)
					return nil
				}

				printf(cmd, "New API token (shown once, store it now):\n\n  %s\n\n", token)
				printf(cmd, "Use it as: Authorization: Bearer <token>\n")
				return nil
			})
		},
	}

	cmd.Flags().BoolVar(&quiet, "quiet", false,
		"print only the token, for piping into a secret store")

	return cmd
}
