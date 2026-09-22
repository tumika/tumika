package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/tumika/tumika/source/daemon/internal/daemon"
	"github.com/tumika/tumika/source/daemon/internal/domain"
)

// configService is the narrow slice of service.ConfigService the CLI is
// allowed to see: reading and writing public settings, never a secret.
// service.ConfigService also exposes ReadSecret/WriteSecret, and those two
// methods have no business reaching this package — see
// agentic/rules/never-log-or-return-a-credential-secret.md.
type configService interface {
	Definitions() []domain.SettingDefinition
	Get(ctx context.Context, key string) (domain.SettingView, error)
	List(ctx context.Context) ([]domain.SettingView, error)
	Set(ctx context.Context, values map[string]json.RawMessage) ([]domain.SettingView, error)
	Reset(ctx context.Context, key string) error
}

// liveDaemonNote is printed after every write, unconditionally: a setting
// value only takes effect where it is read, and some of those reads (server
// startup, provider selection at boot) do not happen again until the daemon
// restarts.
const liveDaemonNote = "note: a running daemon picks this up on its next read; server.listen needs a restart.\n"

// newConfigCmd is the CLI's one in-process exception besides `update`
// (ADR-0013): it must work whether or not a daemon is serving, because
// `config set`/`reset` are how an operator escapes a setting that is the
// reason the daemon will not serve.
func newConfigCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Read and change daemon settings",
	}

	cmd.AddCommand(newConfigListCmd(g))
	cmd.AddCommand(newConfigGetCmd(g))
	cmd.AddCommand(newConfigSetCmd(g))
	cmd.AddCommand(newConfigResetCmd(g))

	return cmd
}

func newConfigListCmd(g *globals) *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List every known setting",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withDaemon(g, cmd, func(d *daemon.Daemon) error {
				return runConfigList(cmd, d.ConfigService(), asJSON)
			})
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "list settings as JSON")
	return cmd
}

func runConfigList(cmd *cobra.Command, cfg configService, asJSON bool) error {
	views, err := cfg.List(cmd.Context())
	if err != nil {
		return err
	}
	if asJSON {
		return encodeJSON(cmd, views)
	}

	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "KEY\tVALUE\tDEFAULT\tIS_SET")
	for _, view := range views {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%t\n",
			view.Key, displayValue(view.Value), displayValue(view.Default), view.IsSet)
	}
	return w.Flush()
}

func newConfigGetCmd(g *globals) *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "get <key>",
		Short: "Print one setting's effective value",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withDaemon(g, cmd, func(d *daemon.Daemon) error {
				return runConfigGet(cmd, d.ConfigService(), args[0], asJSON)
			})
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the setting as JSON")
	return cmd
}

func runConfigGet(cmd *cobra.Command, cfg configService, key string, asJSON bool) error {
	view, err := cfg.Get(cmd.Context(), key)
	if err != nil {
		return err
	}
	if asJSON {
		return encodeJSON(cmd, view)
	}
	printf(cmd, "%s\n", displayValue(view.Value))
	return nil
}

func newConfigSetCmd(g *globals) *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "set <key> <value>",
		Short: "Change one setting",
		Long: "Converts value to the JSON shape its key's kind calls for (a bool setting takes\n" +
			"true or false; everything else is taken as a plain string) and stores it.\n\n" +
			"A bad key or a value that does not fit its kind is rejected by the daemon's\n" +
			"config service, not by this command.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withDaemon(g, cmd, func(d *daemon.Daemon) error {
				return runConfigSet(cmd, d.ConfigService(), args[0], args[1], asJSON)
			})
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the stored setting as JSON")
	return cmd
}

func runConfigSet(cmd *cobra.Command, cfg configService, key, value string, asJSON bool) error {
	views, err := cfg.Set(cmd.Context(), map[string]json.RawMessage{key: encodeShellValue(cfg.Definitions(), key, value)})
	if err != nil {
		return err
	}
	view := views[0]

	if asJSON {
		if err := encodeJSON(cmd, view); err != nil {
			return err
		}
	} else {
		printf(cmd, "%s = %s\n", view.Key, displayValue(view.Value))
	}
	_, _ = fmt.Fprint(cmd.ErrOrStderr(), liveDaemonNote)
	return nil
}

func newConfigResetCmd(g *globals) *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "reset <key>",
		Short: "Remove a setting's stored value, falling back to its default",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withDaemon(g, cmd, func(d *daemon.Daemon) error {
				return runConfigReset(cmd, d.ConfigService(), args[0], asJSON)
			})
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the resulting setting as JSON")
	return cmd
}

// runConfigReset deletes the stored value and then reads it back, rather than
// trusting the default in hand, so the report reflects what Get actually
// returns for an unset key — the same path every other reader of this
// setting takes.
func runConfigReset(cmd *cobra.Command, cfg configService, key string, asJSON bool) error {
	if err := cfg.Reset(cmd.Context(), key); err != nil {
		return err
	}

	view, err := cfg.Get(cmd.Context(), key)
	if err != nil {
		return err
	}

	if asJSON {
		if err := encodeJSON(cmd, view); err != nil {
			return err
		}
	} else {
		printf(cmd, "%s reset to default (%s), no longer explicitly set.\n",
			view.Key, displayValue(view.Default))
	}
	_, _ = fmt.Fprint(cmd.ErrOrStderr(), liveDaemonNote)
	return nil
}

// encodeShellValue converts a plain shell argument to the JSON shape its
// key's kind calls for. An unrecognised key falls through to the string
// case: the encoding is only ever a courtesy, since the service rejects an
// unknown key on its own terms regardless of how the CLI encoded the value.
func encodeShellValue(defs []domain.SettingDefinition, key, value string) json.RawMessage {
	for _, def := range defs {
		if def.Key == key && def.Kind == domain.SettingBool {
			return json.RawMessage(value)
		}
	}
	b, _ := json.Marshal(value)
	return b
}

// displayValue renders a stored JSON value the way an operator typed it: a
// JSON string is unquoted, everything else (true, false, a bare number) is
// shown as JSON already renders it.
func displayValue(raw json.RawMessage) string {
	var str string
	if err := json.Unmarshal(raw, &str); err == nil {
		return str
	}
	return string(raw)
}

// encodeJSON writes v as indented JSON, matching `version --json` and
// `status --json`.
func encodeJSON(cmd *cobra.Command, v any) error {
	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
