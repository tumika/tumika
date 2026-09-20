// Package buildinfo carries the version, commit and build date stamped into the
// binary at release time, plus the runtime facts that go with them.
//
// The values are injected via -ldflags into source/daemon/cmd/tumika (see agentic/tumika-repo.md,
// "Version injection") and handed here by Set before anything else runs. The
// zero state is a development build, which several subsystems short-circuit on:
// self-update refuses to run when IsDev reports true, because a dev build has no
// release to compare against.
package buildinfo

import (
	"fmt"
	"runtime"
)

// DevVersion is the component version of an unreleased build.
const DevVersion = "dev"

// DevRelease is the release label of a build that came from no release. A
// daemon stamped with it has no bill of materials to look itself up in, so it
// counts as older than anything a channel offers.
const DevRelease = "dev"

var current = Info{
	Version:   DevVersion,
	Release:   DevRelease,
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
const PinnedClaudeCodeVersion = "2.1.233"

// Info is the complete build identity of the running binary.
//
// Version is this component's semver and Release is the label of the release
// that shipped it. They are different things: a release carries several
// components, each with its own version, and only the component version is ever
// compared.
type Info struct {
	Version   string `json:"version"`
	Release   string `json:"release"`
	Commit    string `json:"commit"`
	Date      string `json:"date"`
	Go        string `json:"go"`
	Platform  string `json:"platform"`
	ClaudeCLI string `json:"claude_cli"`
	// SchemaVersion is the newest database migration this binary embeds. It is
	// reported so one binary can compare schemas with another before a swap —
	// a binary that embeds fewer migrations than the database has applied
	// cannot run against it.
	SchemaVersion int64 `json:"schema_version"`
}

// Set records the values injected at build time. It is called once from main
// before any other package observes the build info.
//
// schemaVersion is passed in rather than read here: buildinfo is platform code
// and may not import the repository layer, so main — which may — reads the
// embedded migrations and hands the number over.
func Set(version, release, commit, date string, schemaVersion int64) {
	if version != "" {
		current.Version = version
	}
	if release != "" {
		current.Release = release
	}
	if commit != "" {
		current.Commit = commit
	}
	if date != "" {
		current.Date = date
	}
	if schemaVersion > 0 {
		current.SchemaVersion = schemaVersion
	}
}

// Get returns the build identity of the running binary.
func Get() Info { return current }

// Version returns just the component version.
func Version() string { return current.Version }

// Release returns the label of the release this binary was shipped in.
func Release() string { return current.Release }

// SchemaVersion returns the newest database migration this binary embeds.
func SchemaVersion() int64 { return current.SchemaVersion }

// IsDev reports whether this is an unreleased build. Self-update is skipped
// entirely when it is true — there is no release to update from.
func IsDev() bool { return current.Version == DevVersion }

// String renders the one-line form used by `tumika version`.
//
// The second field is the COMPONENT VERSION and nothing else. The updater's
// pre-flight and scripts/verify-release-assets.sh both read it positionally
// from this line, so anything added here goes inside the parentheses.
func (i Info) String() string {
	return fmt.Sprintf("tumika %s (release %s, commit %s, built %s, %s, %s) claude-code %s",
		i.Version, i.Release, i.Commit, i.Date, i.Go, i.Platform, i.ClaudeCLI)
}
