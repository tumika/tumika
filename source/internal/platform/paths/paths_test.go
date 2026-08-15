package paths_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tumika/tumika/source/internal/platform/paths"
)

func TestResolveOverrideWinsOverEnv(t *testing.T) {
	t.Setenv(paths.HomeEnv, "/from/env")

	p, err := paths.Resolve("/from/flag")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if p.Home != "/from/flag" {
		t.Errorf("Home = %q, want the explicit override", p.Home)
	}
}

func TestResolveUsesEnv(t *testing.T) {
	t.Setenv(paths.HomeEnv, "/var/lib/tumika")

	p, err := paths.Resolve("")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if p.Home != "/var/lib/tumika" {
		t.Errorf("Home = %q, want /var/lib/tumika", p.Home)
	}
}

// Every path must sit under Home. The self-updater renames a staged binary into
// its target's own directory, and the whole install is meant to be one tree, so
// a path escaping Home would break both properties.
func TestEverythingLivesUnderHome(t *testing.T) {
	t.Setenv(paths.HomeEnv, "/var/lib/tumika")

	p, err := paths.Resolve("")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	for name, path := range map[string]string{
		"Bin":          p.Bin,
		"Providers":    p.Providers,
		"ClaudeConfig": p.ClaudeConfig,
		"Backups":      p.Backups,
		"Logs":         p.Logs,
		"Run":          p.Run,
		"DB":           p.DB,
		"MasterKey":    p.MasterKey,
	} {
		if !strings.HasPrefix(path, p.Home+string(os.PathSeparator)) {
			t.Errorf("%s = %q, which is not under Home %q", name, path, p.Home)
		}
	}
}

func TestResolveMakesRelativePathsAbsolute(t *testing.T) {
	t.Setenv(paths.HomeEnv, "")

	p, err := paths.Resolve("relative-home")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !filepath.IsAbs(p.Home) {
		t.Errorf("Home = %q, want an absolute path", p.Home)
	}
}

// The tree holds the database, the sealed credentials and the fallback key file,
// so every directory is owner-only.
func TestMkdirAllIsOwnerOnly(t *testing.T) {
	p, err := paths.Resolve(t.TempDir() + "/tumika")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if err := p.MkdirAll(); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	err = filepath.WalkDir(p.Home, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if perm := info.Mode().Perm(); perm != 0o700 {
			t.Errorf("%s has mode %o, want 700", path, perm)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
}

func TestMkdirAllIsIdempotent(t *testing.T) {
	p, err := paths.Resolve(t.TempDir() + "/tumika")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if err := p.MkdirAll(); err != nil {
		t.Fatalf("first MkdirAll: %v", err)
	}
	if err := p.MkdirAll(); err != nil {
		t.Fatalf("second MkdirAll: %v", err)
	}
}

func TestInContainerHonoursEnv(t *testing.T) {
	t.Setenv(paths.HomeEnv, "")
	t.Setenv(paths.ContainerEnv, "1")
	if !paths.InContainer() {
		t.Errorf("InContainer() = false with %s set", paths.ContainerEnv)
	}
}

// A container resolves to a fixed home rather than a user directory: the daemon
// there has no user session, and the data volume is mounted at a known place.
func TestContainerHomeIsFixed(t *testing.T) {
	// TUMIKA_HOME is checked before container detection, so without clearing it
	// this test reads the developer's ambient environment — and TUMIKA_HOME is
	// precisely the variable the systemd unit sets, so anyone who exports it
	// would get a red suite unrelated to their change.
	t.Setenv(paths.HomeEnv, "")
	t.Setenv(paths.ContainerEnv, "1")

	p, err := paths.Resolve("")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if p.Home != "/var/lib/tumika" {
		t.Errorf("Home = %q, want /var/lib/tumika in a container", p.Home)
	}
}

// The override works in both directions. It used to be truthiness-blind:
// TUMIKA_CONTAINER=0 meant "yes", silently relocating the home directory and
// disabling self-update for an operator who had asked for the opposite.
func TestContainerOverrideIsTriState(t *testing.T) {
	tests := map[string]bool{
		"1": true, "true": true, "TRUE": true,
		"0": false, "false": false, "False": false,
		"yes": true, // set but unparseable: taken as intent to force it on
	}

	for value, want := range tests {
		t.Run(value, func(t *testing.T) {
			t.Setenv(paths.ContainerEnv, value)
			if got := paths.InContainer(); got != want {
				t.Errorf("InContainer() with %s=%q = %v, want %v", paths.ContainerEnv, value, got, want)
			}
		})
	}
}

// os.MkdirAll applies its mode only to directories it creates and is a silent
// no-op for one that already exists. A distro package, a Docker VOLUME or an
// admin creating /var/lib/tumika at 0755 would otherwise leave the database, the
// sealed credentials and the fallback master key readable by every user on the
// host — with MkdirAll returning nil.
func TestMkdirAllTightensAPreExistingDirectory(t *testing.T) {
	home := filepath.Join(t.TempDir(), "tumika")
	if err := os.MkdirAll(filepath.Join(home, "bin"), 0o755); err != nil {
		t.Fatalf("pre-create: %v", err)
	}

	p, err := paths.Resolve(home)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if err := p.MkdirAll(); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	for _, dir := range []string{home, filepath.Join(home, "bin")} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("stat %s: %v", dir, err)
		}
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			t.Errorf("%s has mode %#o; the credential tree must not be group- or world-accessible", dir, perm)
		}
	}
}

// An already-correct directory is left alone, so a prepared volume does not
// require chmod permission tumika may not have.
func TestMkdirAllLeavesACorrectDirectoryUntouched(t *testing.T) {
	home := filepath.Join(t.TempDir(), "tumika")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatalf("pre-create: %v", err)
	}

	p, err := paths.Resolve(home)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if err := p.MkdirAll(); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	info, err := os.Stat(home)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("mode = %#o, want 0700 unchanged", perm)
	}
}
