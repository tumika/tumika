// Package buildinfo carries the version, commit and build date stamped into the
// binary at release time, plus the runtime facts that go with them.
//
// The values are injected via -ldflags into source/cmd/tumika (see AGENTS.md,
// "Version injection") and handed here by Set before anything else runs. The
// zero state is a development build, which several subsystems short-circuit on:
// self-update refuses to run when IsDev reports true, because a dev build has no
// release to compare against.
package buildinfo

import (
	"fmt"
	"runtime"
)

// DevVersion is the version string of an unreleased build.
const DevVersion = "dev"

var current = Info{
	Version:   DevVersion,
	Commit:    "none",
	Date:      "unknown",
	Go:        runtime.Version(),
	Platform:  runtime.GOOS + "/" + runtime.GOARCH,
	ClaudeCLI: PinnedClaudeCodeVersion,
}

// PinnedClaudeCodeVersion is the exact Claude Code release tumika installs and
// drives. It is a compile-time constant on purpose: the interactive login flow
// parses this version's terminal output, so changing it is a deliberate, tested
// change rather than a dependency bump.
const PinnedClaudeCodeVersion = "2.1.232"

// Info is the complete build identity of the running binary.
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	Date      string `json:"date"`
	Go        string `json:"go"`
	Platform  string `json:"platform"`
	ClaudeCLI string `json:"claude_cli"`
}

// Set records the values injected at build time. It is called once from main
// before any other package observes the build info.
func Set(version, commit, date string) {
	if version != "" {
		current.Version = version
	}
	if commit != "" {
		current.Commit = commit
	}
	if date != "" {
		current.Date = date
	}
}

// Get returns the build identity of the running binary.
func Get() Info { return current }

// Version returns just the version string.
func Version() string { return current.Version }

// IsDev reports whether this is an unreleased build. Self-update is skipped
// entirely when it is true — there is no release to update from.
func IsDev() bool { return current.Version == DevVersion }

// String renders the one-line form used by `tumika version`.
func (i Info) String() string {
	return fmt.Sprintf("tumika %s (commit %s, built %s, %s, %s) claude-code %s",
		i.Version, i.Commit, i.Date, i.Go, i.Platform, i.ClaudeCLI)
}
