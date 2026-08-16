package cli

import (
	"errors"

	"github.com/spf13/cobra"

	"github.com/tumika/tumika/source/internal/daemon"
	"github.com/tumika/tumika/source/internal/domain"
	"github.com/tumika/tumika/source/internal/platform/buildinfo"
	"github.com/tumika/tumika/source/internal/platform/paths"
	"github.com/tumika/tumika/source/internal/platform/release"
)

// newUpdateCmd updates the tumika binary in place.
//
// One of the few commands that does NOT go through the API (ADR-0004), and for
// a specific reason: the case that matters most is a daemon that will not stay
// up. An operator whose service is crash-looping needs to be able to update it,
// and an HTTP client cannot help them.
func newUpdateCmd(g *globals) *cobra.Command {
	var (
		check   bool
		version string
	)

	cmd := &cobra.Command{
		Use:   "update",
		Short: "Update tumika to the newest release",
		Long: "Downloads the newest published release, verifies it against the release's\n" +
			"checksums, runs it once to confirm it works, and replaces this binary.\n\n" +
			"The previous binary is kept as tumika.old until the new one has booted and\n" +
			"served successfully. If it fails to start three times, the old one is\n" +
			"restored automatically.\n\n" +
			"A running daemon keeps executing the old binary until it restarts — the\n" +
			"replacement is a new file, not a rewrite of the one in memory. Restart the\n" +
			"service to pick it up.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// A development build has no release to update FROM, and the
			// version comparison would be meaningless: "dev" is not a semver,
			// so everything published looks newer and nothing is.
			if buildinfo.IsDev() {
				return errors.New("this is a development build; there is no release to update from")
			}
			if paths.InContainer() {
				return errors.New("self-update is disabled in a container: the image is the unit of deployment, so pull a newer image instead")
			}

			return withDaemon(g, cmd, func(d *daemon.Daemon) error {
				updates := d.UpdateService()
				if updates == nil {
					return errors.New("self-update is disabled for this build")
				}

				available, newer, err := updates.Check(cmd.Context())
				if err != nil {
					if errors.Is(err, release.ErrNoRelease) {
						printf(cmd, "No release has been published yet.\n")
						return nil
					}
					return err
				}

				printf(cmd, "  running    %s\n", buildinfo.Version())
				printf(cmd, "  available  %s\n", available)

				if version == "" {
					if !newer {
						printf(cmd, "\nAlready up to date.\n")
						return nil
					}
					version = available
				}

				if check {
					printf(cmd, "\nRun `tumika update` to install %s.\n", version)
					return nil
				}

				printf(cmd, "\nInstalling %s…\n", version)
				if err := updates.Apply(cmd.Context(), version); err != nil {
					return err
				}

				printf(cmd, "Installed. The previous binary is kept alongside it as tumika.old\n")
				printf(cmd, "Restart the service to run it: tumika stop && tumika start\n")
				return nil
			})
		},
	}

	cmd.Flags().BoolVar(&check, "check", false, "report what is available without installing it")
	cmd.Flags().StringVar(&version, "to", "",
		"install a specific version instead of the newest (must still be newer than the running one)")

	return cmd
}

// newUpdateStatusCmd reports where a self-update has got to.
func newUpdateStatusCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "update-status",
		Short: "Report the state of the last self-update",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withDaemon(g, cmd, func(d *daemon.Daemon) error {
				updates := d.UpdateService()
				if updates == nil {
					printf(cmd, "  status   disabled for this build\n")
					return nil
				}
				state, err := updates.State(cmd.Context())
				if err != nil {
					return err
				}

				printf(cmd, "  status   %s\n", state.Status)
				if state.Status == domain.UpdateIdle {
					return nil
				}
				printf(cmd, "  from     %s\n", state.FromVersion)
				printf(cmd, "  to       %s\n", state.ToVersion)
				if state.Status == domain.UpdatePending {
					printf(cmd, "  boots    %d of %d before rollback\n",
						state.BootAttempts, domain.MaxBootAttempts)
				}
				if state.StartedAt != nil {
					printf(cmd, "  started  %s\n", state.StartedAt.Format("2006-01-02 15:04:05 MST"))
				}
				if state.Status == domain.UpdateRolledBack {
					printf(cmd, "\n  %s failed to boot and the previous binary was restored.\n", state.ToVersion)
					printf(cmd, "  Check the logs from that attempt before trying again.\n")
				}
				return nil
			})
		},
	}
}
