package release

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"golang.org/x/mod/semver"
)

// The repo root, from this package's directory: internal/platform/release is
// five levels below source/daemon's parent.
const repoRoot = "../../../../.."

// The same key scripts/release-label.sh reads: top-level, so column 1.
var releaseKeyPattern = regexp.MustCompile(`(?m)^release:[ \t]*(.*)$`)

func readReleaseYAML(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path) //nolint:gosec // a fixed path under the repo
	if err != nil {
		t.Fatalf("read %s: %v — it is the single source of the release label", path, err)
	}
	m := releaseKeyPattern.FindStringSubmatch(string(data))
	if m == nil {
		t.Fatalf("%s has no top-level 'release:' key", path)
	}
	value := m[1]
	if i := strings.Index(value, "#"); i >= 0 {
		value = value[:i]
	}
	return strings.Trim(strings.TrimSpace(value), `"'`)
}

// release.yaml is the single source of the label every release build is stamped
// with, and the daemon refuses a label it cannot validate — so a file the
// validator rejects is a release whose every update check returns an error.
func TestReleaseYAMLLabelIsValid(t *testing.T) {
	path := filepath.Join(repoRoot, "release.yaml")
	label := readReleaseYAML(t, path)

	if err := ValidateReleaseLabel(label); err != nil {
		t.Fatalf("release.yaml names %q, which the daemon rejects: %v", label, err)
	}
}

// scripts/release-label.sh carries the label pattern in shell, because the
// release workflow needs it before a Go binary exists. Two copies of a pattern
// drift, and the shell one drifting means a release stamped with a label the
// daemon then refuses, so the two are asserted to agree.
func TestReleaseLabelScriptAgreesWithValidator(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is not available")
	}
	script := filepath.Join(repoRoot, "scripts", "release-label.sh")

	labels := []string{
		"2026.09.01",
		"2026.09.01-beta.1",
		"edge.147",
		"1.2.3",
		"2026.9.1",
		"2026.09.01-beta.1234567",
		"2026.09.01-beta.",
		"../x",
	}

	for _, label := range labels {
		t.Run(label, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "release.yaml")
			if err := os.WriteFile(path, []byte("release: "+label+"\n"), 0o600); err != nil {
				t.Fatalf("write %s: %v", path, err)
			}

			out, runErr := exec.Command(bash, script, path).Output() //nolint:gosec // both paths are the test's own
			accepted := runErr == nil
			wantAccepted := ValidateReleaseLabel(label) == nil

			if accepted != wantAccepted {
				t.Fatalf("the script accepted=%v, ValidateReleaseLabel accepted=%v for %q",
					accepted, wantAccepted, label)
			}
			if accepted {
				if got := strings.TrimSpace(string(out)); got != label {
					t.Fatalf("the script printed %q, want %q", got, label)
				}
			}
		})
	}
}

// A missing file is a build with no label at all, so the script has to fail
// rather than print an empty one for the workflow to export.
func TestReleaseLabelScriptRejectsMissingKey(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is not available")
	}
	script := filepath.Join(repoRoot, "scripts", "release-label.sh")

	for name, content := range map[string]string{
		"no key":      "components:\n  daemon: 0.0.1\n",
		"empty key":   "release:\n",
		"two keys":    "release: 2026.09.01\nrelease: 2026.09.02\n",
		"indented":    "spec:\n  release: 2026.09.01\n",
		"commentonly": "release: # 2026.09.01\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "release.yaml")
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatalf("write %s: %v", path, err)
			}
			if err := exec.Command(bash, script, path).Run(); err == nil { //nolint:gosec // both paths are the test's own
				t.Fatalf("the script accepted %q", content)
			}
		})
	}
}

// A component version is semver: the same X.Y.Z core the updater compares,
// which x/mod/semver also accepts in shorthand ("v1", "v1.2") and the release
// files never carry.
func validComponentVersion(v string) bool {
	if !semver.IsValid("v" + v) {
		return false
	}
	core := v
	if i := strings.IndexAny(core, "-+"); i >= 0 {
		core = core[:i]
	}
	return strings.Count(core, ".") == 2
}

// scripts/release-component-version.sh carries the semver pattern in shell, so
// it is asserted to agree with x/mod/semver, which the daemon compares with.
func TestComponentVersionScriptAgreesWithSemver(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is not available")
	}
	script := filepath.Join(repoRoot, "scripts", "release-component-version.sh")

	versions := []string{
		"0.0.1",
		"1.2.3",
		"0.0.2-beta.1",
		"1.0.0-edge.147",
		"1.0.0+build.5",
		"1.0.0-rc.1+build",
		"1.2",
		"1",
		"01.2.3",
		"1.2.3-",
		"1.2.3-beta..1",
		"1.2.3-01",
		"v1.2.3",
		"2026.09.01",
		"x.y.z",
	}

	for _, version := range versions {
		t.Run(version, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "release.yaml")
			content := "release: 2026.09.01\ncomponents:\n  daemon: " + version + "\n"
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatalf("write %s: %v", path, err)
			}

			out, runErr := exec.Command(bash, script, "daemon", path).Output() //nolint:gosec // both paths are the test's own
			accepted := runErr == nil
			want := validComponentVersion(version)

			if accepted != want {
				t.Fatalf("the script accepted=%v, semver accepted=%v for %q", accepted, want, version)
			}
			if accepted {
				if got := strings.TrimSpace(string(out)); got != version {
					t.Fatalf("the script printed %q, want %q", got, version)
				}
			}
		})
	}
}

func TestComponentVersionScriptRejectsMalformedFiles(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is not available")
	}
	script := filepath.Join(repoRoot, "scripts", "release-component-version.sh")

	for name, content := range map[string]string{
		"no block":        "release: 2026.09.01\n",
		"missing":         "components:\n  desktop: 0.1.0\n",
		"empty":           "components:\n  daemon:\n",
		"duplicate":       "components:\n  daemon: 0.0.1\n  daemon: 0.0.2\n",
		"after the block": "components:\n  desktop: 0.1.0\nother:\n  daemon: 0.0.1\n",
		"top level":       "daemon: 0.0.1\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "release.yaml")
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatalf("write %s: %v", path, err)
			}
			if err := exec.Command(bash, script, "daemon", path).Run(); err == nil { //nolint:gosec // both paths are the test's own
				t.Fatalf("the script accepted %q", content)
			}
		})
	}
	if err := exec.Command(bash, script, "daemon", filepath.Join(t.TempDir(), "absent.yaml")).Run(); err == nil { //nolint:gosec // the test's own paths
		t.Fatal("the script accepted a missing file")
	}
}

// The release workflow runs validate-release.sh on the committed file, so the
// file as committed has to pass it, with the tag its label implies and not
// with any other.
func TestReleaseYAMLValidates(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is not available")
	}
	script := filepath.Join(repoRoot, "scripts", "validate-release.sh")
	file := filepath.Join(repoRoot, "release.yaml")
	label := readReleaseYAML(t, file)

	if out, err := exec.Command(bash, script, "--tag", "v"+label, file).CombinedOutput(); err != nil { //nolint:gosec // the test's own paths
		t.Fatalf("release.yaml does not validate: %v\n%s", err, out)
	}
	if err := exec.Command(bash, script, "--tag", "v"+label+"1", file).Run(); err == nil { //nolint:gosec // the test's own paths
		t.Fatal("a tag that is not v<label> validated")
	}
	if err := exec.Command(bash, script, file).Run(); err != nil { //nolint:gosec // the test's own paths
		t.Fatalf("release.yaml without a tag does not validate: %v", err)
	}
}
