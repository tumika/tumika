// Package paths resolves the filesystem layout tumika owns.
//
// Everything tumika writes lives under a single home directory, so that the
// whole state of an install is one tree: the database, the sealed-key fallback,
// the vendored Claude Code versions, the daemon-owned binary, backups and logs.
// That is what makes backup, inspection and support tractable — and it is why
// the self-update scheme can rename a staged binary into place without crossing
// a filesystem boundary (ADR-0003).
package paths

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
)

// HomeEnv overrides the resolved home directory. The systemd unit and the
// container image both set it explicitly rather than relying on detection.
const HomeEnv = "TUMIKA_HOME"

// ContainerEnv overrides container detection, in BOTH directions. Detection is
// a heuristic, and an escape hatch that only forces the behaviour on is half an
// escape hatch: an operator running in a container with a bind-mounted host home
// has an equally real need to turn it off.
const ContainerEnv = "TUMIKA_CONTAINER"

// dirPerm is used for every directory tumika creates. The database, the sealed
// credentials and the fallback key file all live under this tree, so it is
// owner-only throughout.
const dirPerm os.FileMode = 0o700

// ErrUnsupportedPlatform is returned when the home directory cannot be resolved
// because the OS is not one tumika supports (ADR: Linux and macOS only).
var ErrUnsupportedPlatform = errors.New("unsupported platform")

// Paths is the resolved layout. Every field is an absolute path.
type Paths struct {
	// Home is the root of everything tumika owns.
	Home string
	// Bin holds the daemon-owned tumika binary, and its .old predecessor
	// between an update and its confirmation (ADR-0003).
	Bin string
	// Providers holds vendored provider binaries, one directory per version.
	Providers string
	// ClaudeConfig is the isolated CLAUDE_CONFIG_DIR handed to every spawned
	// claude process, so the daemon never reads or mutates the operator's own
	// Claude Code configuration.
	ClaudeConfig string
	// Backups holds the VACUUM INTO snapshots taken before migrations.
	Backups string
	// Logs holds log output when tumika is not writing to a supervisor's journal.
	Logs string
	// Run holds runtime state that does not survive a reboot.
	Run string
	// DB is the SQLite database file.
	DB string
	// MasterKey is the 0600 key file used by the fallback sealing backend
	// (ADR-0002), on platforms with no keystore and in containers.
	MasterKey string
}

// Resolve computes the layout. A non-empty override wins over everything;
// otherwise the home directory comes from TUMIKA_HOME, then from the platform
// default. It creates nothing — call MkdirAll for that.
func Resolve(override string) (Paths, error) {
	home := override
	if home == "" {
		var err error
		if home, err = defaultHome(); err != nil {
			return Paths{}, err
		}
	}

	abs, err := filepath.Abs(home)
	if err != nil {
		return Paths{}, fmt.Errorf("resolve home %q: %w", home, err)
	}

	return Paths{
		Home:         abs,
		Bin:          filepath.Join(abs, "bin"),
		Providers:    filepath.Join(abs, "providers"),
		ClaudeConfig: filepath.Join(abs, "claude"),
		Backups:      filepath.Join(abs, "backups"),
		Logs:         filepath.Join(abs, "logs"),
		Run:          filepath.Join(abs, "run"),
		DB:           filepath.Join(abs, "tumika.db"),
		MasterKey:    filepath.Join(abs, "master.key"),
	}, nil
}

// MkdirAll creates every directory in the layout, owner-only.
func (p Paths) MkdirAll() error {
	for _, dir := range []string{p.Home, p.Bin, p.Providers, p.ClaudeConfig, p.Backups, p.Logs, p.Run} {
		if err := os.MkdirAll(dir, dirPerm); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}
	return nil
}

// defaultHome picks the platform default.
//
// There is deliberately no root-vs-user branch on Linux: the system unit sets
// TUMIKA_HOME=/var/lib/tumika explicitly, so an unprivileged developer run and a
// supervised system install differ by configuration rather than by a euid
// heuristic that would silently relocate the whole install.
func defaultHome() (string, error) {
	if v := os.Getenv(HomeEnv); v != "" {
		return v, nil
	}
	if InContainer() {
		return "/var/lib/tumika", nil
	}

	switch runtime.GOOS {
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("locate user home: %w", err)
		}
		return filepath.Join(home, "Library", "Application Support", "tumika"), nil

	case "linux":
		if v := os.Getenv("XDG_STATE_HOME"); v != "" {
			return filepath.Join(v, "tumika"), nil
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("locate user home: %w", err)
		}
		return filepath.Join(home, ".local", "state", "tumika"), nil

	default:
		return "", fmt.Errorf("%w: %s (tumika supports linux and darwin)", ErrUnsupportedPlatform, runtime.GOOS)
	}
}

// InContainer reports whether tumika is running inside a container. Self-update
// disables itself when it is true: the image is the unit of deployment, and a
// container that rewrites itself no longer matches its tag (ADR-0003).
func InContainer() bool {
	// Parsed rather than tested for emptiness. `TUMIKA_CONTAINER=0` previously
	// meant "yes", which silently relocated the home directory and disabled
	// self-update for someone who had asked for the opposite.
	if v := os.Getenv(ContainerEnv); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
		// Unparseable but set — "yes", "on", "1 " — is taken as intent to force
		// it on. Someone who set the variable at all meant something by it.
		return true
	}
	for _, marker := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := os.Stat(marker); err == nil {
			return true
		}
	}
	return false
}
