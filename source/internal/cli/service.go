package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/tumika/tumika/source/internal/daemon"
	"github.com/tumika/tumika/source/internal/platform/paths"
	"github.com/tumika/tumika/source/internal/platform/secrets"
	"github.com/tumika/tumika/source/internal/platform/servicemgr"
)

// managerFactory builds the platform's service manager. A variable so the
// command tree can be exercised without touching launchctl or systemctl.
var managerFactory = servicemgr.New

// Indirections for the two systemd capabilities install probes. Variables for
// the same reason as managerFactory: the interesting behaviour is what install
// DOES with the answers — seal, or clean up and fall back — and neither answer
// can be produced on a machine without systemd.
var (
	credsUsable   = secrets.SystemdCredsUsable
	handoverWorks = secrets.HandoverWorks
	sealMasterKey = secrets.SealMasterKey
	// installGOOS is the platform install believes it is on.
	installGOOS = runtime.GOOS
)

// newInstallCmd installs tumika as a supervised service.
//
// One of the few commands that does NOT go through the API (ADR-0004): there is
// no daemon yet, which is the entire point of running it.
func newInstallCmd(g *globals) *cobra.Command {
	var (
		binary string
		user   string
	)

	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install tumika as a supervised service",
		Long: "Installs tumika under the platform's service manager: a systemd system unit on\n" +
			"Linux, a LaunchAgent on macOS.\n\n" +
			"The Linux install needs root — it writes /etc/systemd/system and creates the\n" +
			"tumika service account. The macOS install does not.\n\n" +
			"On Linux the service keeps its state in " + paths.SystemHome + ", because the unit runs as\n" +
			"an unprivileged account that cannot reach a per-user directory. Pass --home or\n" +
			"set " + paths.HomeEnv + " to choose somewhere else.\n\n" +
			"Running it again is how an upgrade is applied: the unit is rewritten, reloaded\n" +
			"and restarted onto the new binary, and nothing under the home directory is\n" +
			"touched.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, err := servicePaths(g)
			if err != nil {
				return err
			}

			cfg := servicemgr.Config{
				Binary: binary,
				Home:   p.Home,
				User:   user,
			}
			if cfg.User == "" {
				cfg.User = servicemgr.DefaultUser
			}
			if cfg.Binary == "" {
				cfg.Binary, err = installedBinary(p)
				if err != nil {
					return err
				}
			}

			mgr, err := managerFactory()
			if err != nil {
				return err
			}

			// The service ACCOUNT comes first, before anything that depends on
			// it existing. The handover probe below runs a transient unit as
			// that account, so probing first made systemd-run fail to resolve
			// the user on every first install — the probe reported "no
			// handover", the freshly sealed key was deleted, and the host
			// dropped to the file key permanently.
			if err := mgr.Prepare(cmd.Context(), cfg); err != nil {
				return err
			}

			// Key custody is settled BEFORE the unit is written, because the
			// unit has to name the sealed blob for systemd to hand it over —
			// and sealing needs the root privileges this command already has
			// and the daemon deliberately does not.
			if cfg.SealedKey, err = sealKeyIfSupported(cmd, p, cfg.User); err != nil {
				return err
			}

			// A token is minted BEFORE the service starts. The daemon refuses
			// to serve without one, and Restart=always turns that refusal into
			// a crash loop — so a fresh install would report success and leave
			// systemd restarting a failing unit every five seconds forever.
			// Found by running the install under a real systemd.
			token, err := ensureAPIToken(g, cmd)
			if err != nil {
				return err
			}
			// Printed as soon as it exists, not at the end. Its hash is already
			// stored, so an install that fails after this point would otherwise
			// leave a daemon with a token nobody has ever seen — and a re-run
			// finds one configured and prints nothing.
			printToken(cmd, token)

			if err := mgr.Install(cmd.Context(), cfg); err != nil {
				return err
			}

			printf(cmd, "Installed and started.\n")
			printf(cmd, "  binary  %s\n", cfg.Binary)
			printf(cmd, "  home    %s\n", cfg.Home)
			if cfg.SealedKey != "" {
				printf(cmd, "  key     sealed to this host (%s)\n", cfg.SealedKey)
			}
			return reportStatus(cmd, mgr)
		},
	}

	cmd.Flags().StringVar(&binary, "binary", "",
		"path to the tumika binary the service runs (default: the daemon-owned copy under the home directory)")
	cmd.Flags().StringVar(&user, "user", "",
		"account to run as on Linux (default: "+servicemgr.DefaultUser+")")

	return cmd
}

// servicePaths resolves the layout a SUPERVISED install should use.
//
// On Linux that is the system-wide home, not the XDG per-user one. The default
// is right for a developer running the daemon in a terminal and wrong here: the
// unit runs as an unprivileged account, and `sudo tumika install` resolves
// root's HOME, so the unit ends up naming /root/.local/state/tumika. The account
// cannot traverse a 0700 /root, systemd answers 203/EXEC, and Restart=always
// turns it into a loop the install reports as running.
//
// An explicit --home or TUMIKA_HOME still wins, because an operator who said
// where meant it.
func servicePaths(g *globals) (paths.Paths, error) {
	if g.home == "" && os.Getenv(paths.HomeEnv) == "" && installGOOS == "linux" {
		return paths.Resolve(paths.SystemHome)
	}
	return g.Paths()
}

// printToken writes a freshly minted token to stdout, and nowhere else.
//
// Never through the logger, which would put a live credential into the journal,
// and never into a file — the only copy is the one the operator keeps.
func printToken(cmd *cobra.Command, token string) {
	if token == "" {
		return
	}
	printf(cmd, "\nYour API token (shown once, only its hash is stored):\n\n  %s\n\n", token)
}

// sealKeyIfSupported seals a host-bound master key, when the host can.
//
// Returns the blob's path, or "" to leave the daemon on its file-based
// fallback — which is what a container gets, and what a non-systemd
// distribution gets.
//
// Only ever ADDS custody. If a blob already exists it is reused untouched:
// replacing it would orphan every credential sealed under the key inside.
func sealKeyIfSupported(cmd *cobra.Command, p paths.Paths, user string) (string, error) {
	if installGOOS != "linux" {
		return "", nil
	}

	sealed := secrets.CredentialPathFor(p.MasterKey)
	if _, err := os.Stat(sealed); err == nil {
		return sealed, nil
	}

	// A daemon already running on a file key must not be moved: the two keys
	// are unrelated, and switching custody would make every stored credential
	// unreadable while reporting a healthy start.
	if _, err := os.Stat(p.MasterKey); err == nil {
		printf(cmd, "Keeping the existing file-based master key; credentials are already sealed under it.\n")
		return "", nil
	}

	if !credsUsable(cmd.Context(), nil) {
		return "", nil
	}
	if err := sealMasterKey(cmd.Context(), sealed, nil); err != nil {
		return "", err
	}

	// Sealing a key and RECEIVING one are different capabilities, and a host
	// can have the first without the second. Proved with a transient unit that
	// reads the credential exactly as the real one will, because committing to
	// this custody without checking leaves a daemon that can never start —
	// which is precisely what happened the first time, in a container where
	// systemd sets $CREDENTIALS_DIRECTORY to a directory it never creates.
	// Probed as the account the unit will ACTUALLY run as, which is not
	// necessarily the default: --user assistant would otherwise verify delivery
	// to an account that never runs the service.
	if !handoverWorks(cmd.Context(), sealed, user, nil) {
		// Nothing is sealed under this key yet — it was minted seconds ago — so
		// removing it is safe, and leaving it behind would make every later
		// start fail closed on a key nobody can use.
		if err := os.Remove(sealed); err != nil {
			return "", fmt.Errorf("remove the unusable sealed key %s: %w", sealed, err)
		}
		printf(cmd, "This host can seal a key but systemd does not deliver credentials to services here,\n")
		printf(cmd, "so tumika will use its file-based key instead. That key sits beside the database:\n")
		printf(cmd, "set %s if you need custody elsewhere.\n", secrets.MasterKeyEnv)
		return "", nil
	}
	return sealed, nil
}

// ensureAPIToken mints a token when there is none, and returns it so install can
// print it. An existing token is left alone and "" comes back: rotating on every
// install would break every client an operator already configured.
func ensureAPIToken(g *globals, cmd *cobra.Command) (string, error) {
	var token string

	err := withDaemon(g, cmd, func(d *daemon.Daemon) error {
		configured, err := d.AuthService().Configured(cmd.Context())
		if err != nil {
			return err
		}
		if configured {
			return nil
		}
		token, err = d.AuthService().Rotate(cmd.Context())
		return err
	})
	return token, err
}

// installedBinary is the copy the service will run.
//
// The daemon owns its own binary under the home directory (ADR-0003), because
// an update is an atomic rename followed by exit — and a non-root account cannot
// rewrite a root-owned file in /usr/local/bin. So install COPIES the running
// executable into place rather than pointing the unit at wherever it happens to
// be today, which might be a download directory that is gone next week.
func installedBinary(p paths.Paths) (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate the running binary: %w", err)
	}
	self, err = filepath.EvalSymlinks(self)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", self, err)
	}

	target := filepath.Join(p.Bin, "tumika")
	if self == target {
		return target, nil
	}

	if err := os.MkdirAll(p.Bin, 0o700); err != nil {
		return "", fmt.Errorf("create %s: %w", p.Bin, err)
	}
	if err := copyExecutable(self, target); err != nil {
		return "", err
	}
	return target, nil
}

// copyExecutable writes src to dst atomically.
//
// Staged and renamed rather than written in place: the destination may be the
// binary a running daemon is executing, and truncating that is how a service
// dies mid-write with nothing to restart onto.
func copyExecutable(src, dst string) error {
	data, err := os.ReadFile(src) // #nosec G304 -- src is this process's own resolved executable path
	if err != nil {
		return fmt.Errorf("read %s: %w", src, err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(dst), ".tumika.*")
	if err != nil {
		return fmt.Errorf("stage a copy in %s: %w", filepath.Dir(dst), err)
	}
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}()

	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write %s: %w", tmp.Name(), err)
	}
	// 0755, not 0700: on Linux the unit runs as the tumika account while the
	// installer is root, so an owner-only binary is one the service cannot
	// execute. It is a public executable, and it holds nothing secret.
	if err := tmp.Chmod(0o755); err != nil { // #nosec G302 -- must be executable by the service account, and contains no secret
		return fmt.Errorf("set permissions on %s: %w", tmp.Name(), err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("flush %s: %w", tmp.Name(), err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmp.Name(), err)
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return fmt.Errorf("install %s: %w", dst, err)
	}
	return nil
}

func newUninstallCmd(_ *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall",
		Short: "Remove the tumika service",
		Long: "Stops the service and removes its unit or LaunchAgent.\n\n" +
			"Nothing under the tumika home directory is deleted: the database, the sealed\n" +
			"credentials and the vendored provider binaries are all left in place, so\n" +
			"reinstalling picks up where this left off. Remove the home directory by hand\n" +
			"if that is what you actually want.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			mgr, err := managerFactory()
			if err != nil {
				return err
			}
			if err := mgr.Uninstall(cmd.Context()); err != nil {
				return err
			}
			printf(cmd, "Removed. Your data is untouched.\n")
			return nil
		},
	}
}

func newStartCmd(_ *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "start",
		Short: "Start the tumika service",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			mgr, err := managerFactory()
			if err != nil {
				return err
			}
			if err := mgr.Start(cmd.Context()); err != nil {
				return err
			}
			return reportStatus(cmd, mgr)
		},
	}
}

func newStopCmd(_ *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop the tumika service",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			mgr, err := managerFactory()
			if err != nil {
				return err
			}
			if err := mgr.Stop(cmd.Context()); err != nil {
				return err
			}
			return reportStatus(cmd, mgr)
		},
	}
}

func newStatusCmd(_ *globals) *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Report whether the tumika service is installed and running",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			mgr, err := managerFactory()
			if err != nil {
				return err
			}
			status, err := mgr.Status(cmd.Context())
			if err != nil {
				return err
			}

			if asJSON {
				encoded, err := json.MarshalIndent(status, "", "  ")
				if err != nil {
					return err
				}
				printf(cmd, "%s\n", encoded)
			} else {
				printStatus(cmd, status)
			}

			// A stopped service is a fact, not a command failure: `tumika
			// status` in a shell script should not need `|| true` to report one.
			return nil
		},
	}

	cmd.Flags().BoolVar(&asJSON, "json", false, "print the status as JSON")
	return cmd
}

func reportStatus(cmd *cobra.Command, mgr servicemgr.Manager) error {
	status, err := mgr.Status(cmd.Context())
	if err != nil {
		return err
	}
	printStatus(cmd, status)
	return nil
}

func printStatus(cmd *cobra.Command, status servicemgr.Status) {
	printf(cmd, "  manager %s\n", status.Manager)
	printf(cmd, "  state   %s\n", status.State)

	if status.State == servicemgr.StateNotInstalled {
		printf(cmd, "  run `tumika install` to set it up\n")
		return
	}

	printf(cmd, "  at boot %s\n", yesNo(status.Enabled))
	printf(cmd, "  unit    %s\n", status.Path)
	if status.Detail != "" {
		printf(cmd, "  detail  %s\n", status.Detail)
	}
	if status.State == servicemgr.StateStarting {
		printf(cmd, "\n  Starting. If it stays here it is restarting in a loop — check the logs.\n")
	}
	if status.State == servicemgr.StateFailed {
		printf(cmd, "\n  The service is not running. Check the logs for why.\n")
	}
	if status.State == servicemgr.StateRunning && !status.Enabled {
		// Worth saying out loud: it works now and disappears at the next
		// reboot, which is the kind of thing discovered months later.
		printf(cmd, "\n  Running, but not enabled at boot — it will not come back after a restart.\n")
	}
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
