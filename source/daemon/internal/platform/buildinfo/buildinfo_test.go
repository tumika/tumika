package buildinfo_test

import (
	"strings"
	"testing"

	"github.com/tumika/tumika/source/daemon/internal/platform/buildinfo"
)

// The updater's pre-flight and scripts/verify-release-assets.sh both read the
// component version positionally out of this line. Everything else this line
// reports goes inside the parentheses, behind that field.
func TestStringKeepsTheComponentVersionInFieldTwo(t *testing.T) {
	info := buildinfo.Info{
		Version:       "0.0.2",
		Release:       "2026.09.00",
		Commit:        "abc1234",
		Date:          "2026-09-20T07:28:00Z",
		Go:            "go1.25.0",
		Platform:      "darwin/arm64",
		ClaudeCLI:     buildinfo.PinnedClaudeCodeVersion,
		SchemaVersion: 7,
	}

	line := info.String()

	// What parseVersion in service/update.go does with this line: split the
	// first line on whitespace, check field 0, take field 1. Replicated here
	// because it is unexported, and this line is its entire evidence.
	fields := strings.Fields(line)
	if len(fields) < 2 || fields[0] != "tumika" {
		t.Fatalf("output is not parseable as `tumika <version> …`: %q", line)
	}
	if fields[1] != "0.0.2" {
		t.Errorf("field 2 = %q, want the component version: %q", fields[1], line)
	}

	for _, want := range []string{"(release 2026.09.00,", "commit abc1234,", "claude-code "} {
		if !strings.Contains(line, want) {
			t.Errorf("output is missing %q: %q", want, line)
		}
	}
}

// A build nobody stamped is a development build, and the release label it
// carries is what an update check treats as "older than anything".
func TestUnstampedValuesDefaultToDev(t *testing.T) {
	if buildinfo.DevVersion != "dev" || buildinfo.DevRelease != "dev" {
		t.Fatalf("DevVersion = %q, DevRelease = %q", buildinfo.DevVersion, buildinfo.DevRelease)
	}

	// Empty means "keep what is there", so an ldflag that did not take leaves
	// the dev defaults rather than blanking the build identity.
	before := buildinfo.Get()
	buildinfo.Set("", "", "", "", 0)
	if buildinfo.Get() != before {
		t.Errorf("Set with empty values changed the build info: %+v", buildinfo.Get())
	}
}

func TestSetRecordsTheInjectedValues(t *testing.T) {
	before := buildinfo.Get()
	t.Cleanup(func() {
		buildinfo.Set(before.Version, before.Release, before.Commit, before.Date, before.SchemaVersion)
	})

	buildinfo.Set("0.0.2", "2026.09.00", "abc1234", "2026-09-20T07:28:00Z", 7)

	if buildinfo.Version() != "0.0.2" {
		t.Errorf("Version() = %q", buildinfo.Version())
	}
	if buildinfo.Release() != "2026.09.00" {
		t.Errorf("Release() = %q", buildinfo.Release())
	}
	if buildinfo.SchemaVersion() != 7 {
		t.Errorf("SchemaVersion() = %d", buildinfo.SchemaVersion())
	}
	if buildinfo.IsDev() {
		t.Error("IsDev() is true for a stamped build, which would disable self-update")
	}
}
