package cli

import (
	"encoding/json"

	"github.com/spf13/cobra"

	"github.com/tumika/tumika/source/internal/platform/buildinfo"
)

func newVersionCmd(g *globals) *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print build information",
		Long: "Print the version, commit and build date of this binary.\n\n" +
			"The updater execs `tumika version` on a freshly staged binary and asserts the\n" +
			"semver matches before replacing the live one, so this command's output is part\n" +
			"of the update contract (ADR-0003).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			info := buildinfo.Get()

			if asJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(info)
			}

			printf(cmd, "%s\n", info)
			printf(cmd, "home: %s\n", g.paths.Home)
			return nil
		},
	}

	cmd.Flags().BoolVar(&asJSON, "json", false, "print build information as JSON")

	return cmd
}
