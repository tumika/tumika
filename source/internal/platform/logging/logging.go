// Package logging builds tumika's slog logger.
//
// Every logger this package returns is wrapped in the redaction handler, and
// there is no constructor that skips it. tumika runs as a daemon, so its output
// goes to a journal or a log file and from there into backups, support bundles
// and issue reports; a subscription OAuth token is valid for roughly a year and
// cannot be scoped or revoked selectively. See
// .agents/rules/never-log-or-return-a-credential-secret.md.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
)

// Format selects the output encoding.
type Format string

const (
	// FormatText is human-readable output, the default for an interactive CLI.
	FormatText Format = "text"
	// FormatJSON is structured output, the default under a supervisor.
	FormatJSON Format = "json"
)

// Options configures the logger.
type Options struct {
	// Level is one of debug, info, warn, error. Empty means info.
	Level string
	// Format is text or json. Empty means text.
	Format Format
	// Output defaults to os.Stderr.
	Output io.Writer
	// AddSource includes the calling file and line. Useful with debug.
	AddSource bool
}

// New builds a logger. It does not install it as the default.
func New(opts Options) (*slog.Logger, error) {
	level, err := ParseLevel(opts.Level)
	if err != nil {
		return nil, err
	}

	out := opts.Output
	if out == nil {
		out = os.Stderr
	}

	handlerOpts := &slog.HandlerOptions{Level: level, AddSource: opts.AddSource}

	var base slog.Handler
	switch Format(strings.ToLower(string(opts.Format))) {
	case FormatJSON:
		base = slog.NewJSONHandler(out, handlerOpts)
	case FormatText, "":
		base = slog.NewTextHandler(out, handlerOpts)
	default:
		return nil, fmt.Errorf("unknown log format %q (want text or json)", opts.Format)
	}

	return slog.New(NewHandler(base)), nil
}

// Setup builds a logger and installs it as slog's default, so that any package
// reaching for slog.Default gets the redacting one rather than the stdlib's.
func Setup(opts Options) (*slog.Logger, error) {
	logger, err := New(opts)
	if err != nil {
		return nil, err
	}
	slog.SetDefault(logger)
	return logger, nil
}

// ParseLevel maps a level name to a slog.Level. An empty name means info.
func ParseLevel(name string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info", "":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("unknown log level %q (want debug, info, warn or error)", name)
	}
}
