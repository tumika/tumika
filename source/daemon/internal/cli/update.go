package cli

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/tumika/tumika/source/daemon/internal/daemon"
	"github.com/tumika/tumika/source/daemon/internal/domain"
	"github.com/tumika/tumika/source/daemon/internal/platform/buildinfo"
	"github.com/tumika/tumika/source/daemon/internal/platform/paths"
	"github.com/tumika/tumika/source/daemon/internal/platform/release"
	"github.com/tumika/tumika/source/daemon/internal/platform/servicemgr"
	"github.com/tumika/tumika/source/daemon/internal/service"
)

// executablePath resolves the binary this process is executing, symlinks and
// all. A variable so a restart can be exercised without building a binary in
// the place the service would run it from.
var executablePath = service.BinaryPath

// newUpdateCmd updates the tumika binary in place.
//
// One of the few commands that does NOT go through the API (ADR-0004), and for
// a specific reason: the case that matters most is a daemon that will not stay
// up. An operator whose service is crash-looping needs to be able to update it,
// and an HTTP client cannot help them.
func newUpdateCmd(g *globals) *cobra.Command {
	var (
		check     bool
		version   string
		noRestart bool
	)

	cmd := &cobra.Command{
		Use:   "update",
		Short: "Update tumika to the newest release",
		Long: "Downloads what the update.channel setting's channel currently offers, verifies\n" +
			"it against the signed bill of materials, runs it once to confirm it works, and\n" +
			"replaces this binary.\n\n" +
			"The previous binary is kept as tumika.old until the new one has booted and\n" +
			"served successfully. If it fails to start three times, the old one is\n" +
			"restored automatically.\n\n" +
			"A running daemon keeps executing the old binary until it restarts — the\n" +
			"replacement is a new file, not a rewrite of the one in memory. So when the\n" +
			"service is installed and running, this stops and starts it onto the new\n" +
			"binary. Pass --no-restart to leave the daemon on the build it is running.",
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

			// The same layout install uses, so the managed binary below is the
			// one the supervisor was pointed at.
			p, err := servicePaths(g)
			if err != nil {
				return err
			}

			return withDaemon(g, cmd, func(d *daemon.Daemon) error {
				return runUpdate(cmd, d.UpdateService(), updateOptions{
					current:   buildinfo.Version(),
					check:     check,
					version:   version,
					noRestart: noRestart,
					managed:   filepath.Join(p.Bin, "tumika"),
				})
			})
		},
	}

	cmd.Flags().BoolVar(&check, "check", false, "report what is available without installing it")
	cmd.Flags().StringVar(&version, "to", "",
		"install a specific component version (it must be the one the channel's head ships)")
	cmd.Flags().BoolVar(&noRestart, "no-restart", false,
		"leave the service running the old binary instead of restarting it onto the new one")

	return cmd
}

// updateOptions is everything runUpdate needs besides the service itself.
type updateOptions struct {
	// current is the running build's component version, for the report.
	current string
	// check reports what is available and installs nothing.
	check bool
	// version pins a component version, instead of the channel's head.
	version string
	// noRestart leaves the daemon on the build it is executing.
	noRestart bool
	// managed is the binary the supervisor runs — the daemon-owned copy under
	// the home directory. A restart only reaches the new build when that is the
	// file this command replaced.
	managed string
}

// runUpdate is the update command's body, separated from the command so it can
// be tested.
//
// Extracting it is not merely convenient: the guards above key on
// buildinfo.IsDev(), which is always true in a test binary, so everything below
// them was unreachable — the command was covered at 20% and the untested part
// was the part that installs software.
func runUpdate(cmd *cobra.Command, updates service.UpdateService, opts updateOptions) error {
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

	printf(cmd, "  running    %s\n", opts.current)
	printf(cmd, "  available  %s\n", available)

	version := opts.version
	if version == "" {
		if !newer {
			printf(cmd, "\nAlready up to date.\n")
			return nil
		}
		version = available
	}

	if opts.check {
		printf(cmd, "\nRun `tumika update` to install %s.\n", version)
		return nil
	}

	printf(cmd, "\nInstalling %s…\n", version)
	if err := updates.Apply(cmd.Context(), version); err != nil {
		return err
	}

	printf(cmd, "Installed. The previous binary is kept alongside it as tumika.old\n")

	if opts.noRestart {
		printf(cmd, "Not restarting. The daemon runs the old binary until it does: tumika stop && tumika start\n")
		return nil
	}
	return restartService(cmd, opts.managed)
}

// restartService puts a running daemon onto the binary that was just installed.
//
// A process cannot replace the binary it is executing and keep running
// (ADR-0003), so the swap is only half an update until the supervisor
// re-executes. Everything that is not an outright restart failure is reported
// and left alone: the new binary is on disk either way, and an operator who has
// no service installed has nothing to fix.
func restartService(cmd *cobra.Command, managed string) error {
	self, err := executablePath()
	if err != nil {
		warnNoRestart(cmd, fmt.Sprintf("the running binary could not be located: %v", err))
		return nil
	}

	// The supervisor runs the managed copy, so restarting after replacing some
	// other tumika would relaunch the daemon on exactly the build it already
	// has — a restart that reports success and changes nothing.
	// self is symlink-resolved, so managed is resolved too: a symlinked home or
	// data volume names the same file by two strings.
	resolved := managed
	if r, err := filepath.EvalSymlinks(managed); err == nil {
		resolved = r
	}
	if self != resolved {
		warnNoRestart(cmd, fmt.Sprintf("%s was replaced, but the service runs %s", self, managed))
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
			"Run `tumika install` to point the service at this copy.\n")
		return nil
	}

	mgr, err := managerFactory()
	if err != nil {
		warnNoRestart(cmd, fmt.Sprintf("there is no service manager here: %v", err))
		return nil
	}
	status, err := mgr.Status(cmd.Context())
	if err != nil {
		warnNoRestart(cmd, fmt.Sprintf("the service's state could not be read: %v", err))
		return nil
	}

	if status.State == servicemgr.StateNotInstalled {
		printf(cmd, "tumika is not installed as a service, so there is nothing to restart.\n")
		return nil
	}
	// Only a service the supervisor has up is restarted. Anything else — stopped,
	// failed, or stuck activating in a crash loop — is a state the operator
	// chose or is already investigating, and starting it from under them turns
	// an update into a deployment.
	if status.State != servicemgr.StateRunning {
		printf(cmd, "The service is %s, so it was not restarted. Start it with: tumika start\n", status.State)
		return nil
	}

	printf(cmd, "Restarting the service…\n")
	if err := mgr.Stop(cmd.Context()); err != nil {
		return fmt.Errorf("stop the service: %w", err)
	}
	if err := mgr.Start(cmd.Context()); err != nil {
		// The one outcome an operator must never have to infer: the binary is
		// replaced and NOTHING is serving. Said plainly, and the command exits
		// non-zero so a script notices too.
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
			"The service is STOPPED and did not start again: %v\n", err)
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
			"The new binary is in place. Start it with: tumika start\n")
		return fmt.Errorf("start the service: %w", err)
	}

	return reportStatus(cmd, mgr)
}

// warnNoRestart reports why the service was left on the old binary. Stderr,
// because the update itself succeeded and the report on stdout is still true.
func warnNoRestart(cmd *cobra.Command, reason string) {
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
		"Warning: the service was not restarted: %s\n", reason)
}

// newUpdateStatusCmd reports where a self-update has got to.
func newUpdateStatusCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "update-status",
		Short: "Report the state of the last self-update",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withDaemon(g, cmd, func(d *daemon.Daemon) error {
				return runUpdateStatus(cmd, d.UpdateService())
			})
		},
	}
}

// runUpdateStatus is the status command's body, separated for the same reason.
func runUpdateStatus(cmd *cobra.Command, updates service.UpdateService) error {
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
}
