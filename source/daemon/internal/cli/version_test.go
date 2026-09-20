package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tumika/tumika/source/daemon/internal/platform/buildinfo"
	"github.com/tumika/tumika/source/daemon/internal/platform/paths"
)

// The updater execs `<staged> version` and reads the component version out of
// field 2 before it replaces the live binary (ADR-0003);
// scripts/verify-release-assets.sh reads the same field. The release goes
// inside the parentheses so that field stays where both of them look.
func TestVersionTextKeepsTheComponentVersionInFieldTwo(t *testing.T) {
	t.Setenv(paths.HomeEnv, "/var/lib/tumika")

	out, _, err := run(t, "version")
	if err != nil {
		t.Fatalf("version: %v", err)
	}

	// What parseVersion in service/update.go does: take the first line, split
	// on whitespace, check field 0, read field 1. Replicated because it is
	// unexported.
	first, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	fields := strings.Fields(first)
	if len(fields) < 2 || fields[0] != "tumika" {
		t.Fatalf("output is not parseable as `tumika <version> …`: %q", first)
	}
	if fields[1] != buildinfo.Version() {
		t.Errorf("field 2 = %q, want the component version %q", fields[1], buildinfo.Version())
	}
	if !strings.Contains(first, "(release "+buildinfo.Release()+",") {
		t.Errorf("the release is missing from %q", first)
	}
}

// --json is the machine-readable half of the same contract: the updater reads
// the schema version from it to refuse a binary that embeds fewer migrations
// than the database has applied.
func TestVersionJSONCarriesTheReleaseAndTheSchemaVersion(t *testing.T) {
	t.Setenv(paths.HomeEnv, "/var/lib/tumika")

	out, _, err := run(t, "version", "--json")
	if err != nil {
		t.Fatalf("version --json: %v", err)
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &fields); err != nil {
		t.Fatalf("not valid JSON (%v): %s", err, out)
	}

	for _, key := range []string{"version", "release", "commit", "date", "schema_version", "claude_cli"} {
		if _, ok := fields[key]; !ok {
			t.Errorf("--json is missing %q: %s", key, out)
		}
	}

	// The channel is a daemon setting, and this command answers without a
	// daemon and without opening the database — which is exactly what makes it
	// usable as a pre-flight. Asking the running daemon is GET /v1/version.
	if _, ok := fields["channel"]; ok {
		t.Errorf("--json reports a channel it cannot know: %s", out)
	}
}
