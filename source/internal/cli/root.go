// Package cli implements tumika's command-line interface.
//
// The CLI is an HTTP client of the daemon, not a second entry point into the
// services (ADR-0004). Commands that act on daemon state talk to the API; only
// the commands that must run before a daemon exists — install, version — touch
// the local machine directly.
package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/tumika/tumika/source/internal/platform/buildinfo"
	"github.com/tumika/tumika/source/internal/platform/logging"
	"github.com/tumika/tumika/source/internal/platform/paths"
)

// globals holds what every command needs, populated once in the root command's
// PersistentPreRunE so that subcommands never re-parse flags or re-resolve the
// layout.
type globals struct {
	home      string
	logLevel  string
	logFormat string

	resolved    paths.Paths
	resolvedErr error
	didResolve  bool
	logger      *slog.Logger
}

// Paths resolves the filesystem layout on first use.
//
// Deliberately lazy. Resolving in PersistentPreRunE meant every command failed
// when the home directory could not be determined — including `version`, which
// needs no paths at all. That is not just a developer-ergonomics problem: the
// updater execs `<staged> version` to assert the semver before replacing the
// live binary (ADR-0003), and a supervised daemon may have no HOME set. A
// pre-flight that cannot run is an update that cannot happen.
func (g *globals) Paths() (paths.Paths, error) {
	if !g.didResolve {
		g.resolved, g.resolvedErr = paths.Resolve(g.home)
		g.didResolve = true
	}
	return g.resolved, g.resolvedErr
}

// Execute runs the CLI and returns the process exit code. ctx is cancelled on
// SIGINT/SIGTERM, so a command that respects it shuts down gracefully.
func Execute(ctx context.Context) int {
	return execute(ctx, newRootCmd())
}

// execute is Execute with the command injected, so the exit-code and
// error-reporting rules can be tested without building the real command tree.
func execute(ctx context.Context, cmd *cobra.Command) int {
	if err := cmd.ExecuteContext(ctx); err != nil {
		// Cancellation is how a graceful shutdown ends. Reported as neither an
		// error line nor a non-zero exit: a journal that logs "Error: context
		// canceled" on every clean stop teaches operators to ignore the word,
		// and any log-scraping alert fires on every restart.
		if errors.Is(err, context.Canceled) {
			return 0
		}
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "Error:", err)
		return 1
	}
	return 0
}

func newRootCmd() *cobra.Command {
	g := &globals{}

	cmd := &cobra.Command{
		Use:   "tumika",
		Short: "A self-hostable personal assistant daemon",
		Long: "tumika runs deterministic workflows on a schedule or on events.\n\n" +
			"It installs and supervises itself, serves a token-authenticated HTTP API,\n" +
			"and installs and authenticates the LLM providers it drives.",
		Version:      buildinfo.Version(),
		SilenceUsage: true,
		// Errors are printed by Execute, not by cobra: cobra prints before
		// returning, so a cancelled context produced an "Error:" line on stderr
		// AND exit code 0. See Execute.
		SilenceErrors: true,
		// Errors are returned, not printed and exited, so Execute owns the exit
		// code and a cancelled context can be distinguished from a real failure.
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			return g.setup(cmd)
		},
	}

	flags := cmd.PersistentFlags()
	flags.StringVar(&g.home, "home", "",
		"tumika home directory (overrides $"+paths.HomeEnv+" and the platform default)")
	flags.StringVar(&g.logLevel, "log-level", "info", "log level: debug, info, warn, error")
	flags.StringVar(&g.logFormat, "log-format", string(logging.FormatText), "log format: text or json")

	cmd.SetOut(os.Stdout)
	cmd.SetErr(os.Stderr)

	cmd.AddCommand(newVersionCmd(g))
	cmd.AddCommand(newServeCmd(g))
	cmd.AddCommand(newTokenCmd(g))
	cmd.AddCommand(newInstallCmd(g))
	cmd.AddCommand(newUninstallCmd(g))
	cmd.AddCommand(newStartCmd(g))
	cmd.AddCommand(newStopCmd(g))
	cmd.AddCommand(newStatusCmd(g))
	cmd.AddCommand(newUpdateCmd(g))
	cmd.AddCommand(newUpdateStatusCmd(g))

	return cmd
}

// setup installs the redacting logger, and nothing else. Path resolution is
// deferred to Paths so that a command which does not need the filesystem cannot
// be broken by it.
func (g *globals) setup(cmd *cobra.Command) error {
	logger, err := logging.Setup(logging.Options{
		Level:  g.logLevel,
		Format: logging.Format(g.logFormat),
		Output: cmd.ErrOrStderr(),
	})
	if err != nil {
		return err
	}
	g.logger = logger

	return nil
}

// printf writes to the command's configured stdout.
//
// errcheck is on, and a failed write to stdout is not something a CLI command
// can act on — the discard is explicit here, once, rather than at every call
// site.
func printf(cmd *cobra.Command, format string, args ...any) {
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), format, args...)
}
