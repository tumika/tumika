// Package servicemgr installs tumika as a supervised service.
//
// Two implementations, one per platform idiom (ADR: D2). Linux gets a SYSTEM
// unit with User=tumika rather than a `systemd --user` one, because the Pi this
// is written for has no interactive login and a user manager would need
// `loginctl enable-linger` to survive a reboot. macOS gets a LaunchAgent rather
// than a LaunchDaemon, because key custody there is the login Keychain and a
// daemon has no session to reach it from.
//
// NEITHER is selected by build tag. Both compile on both platforms and only
// New() consults runtime.GOOS — so the systemd manager's unit rendering and
// command sequencing are testable on a Mac, which is where this was written and
// where nobody can run systemctl. A build tag would have made the Linux path
// verifiable only on Linux, which for a Pi deployment means "only in CI, only at
// the end".
package servicemgr

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Label identifies the service to the OS. Same string on both platforms so an
// operator reading a support answer finds it either way.
const Label = "com.tumika.daemon"

// UnitName is the systemd unit's filename.
const UnitName = "tumika.service"

// DefaultUser is the account the Linux unit runs as.
//
// A dedicated system account, not root and not the installing operator: the
// daemon owns its own binary and rewrites it during an update (ADR-0003), and
// anything with a shell that can write that binary is a privilege escalation
// waiting to be found.
const DefaultUser = "tumika"

// State is what the supervisor says about the service right now.
type State string

const (
	// StateNotInstalled means no unit or plist exists.
	StateNotInstalled State = "not_installed"
	// StateStopped means installed but not running.
	StateStopped State = "stopped"
	// StateRunning means the supervisor has it up.
	StateRunning State = "running"
	// StateStarting means the supervisor is trying, and has not got there yet.
	//
	// Deliberately NOT folded into running. "activating" is what a unit stuck in
	// a Restart=always crash loop reports forever — it never reaches "failed",
	// because the supervisor keeps trying — so calling it running is the exact
	// lie that made a dead install report success.
	StateStarting State = "starting"
	// StateFailed means it is installed, not running, and the supervisor
	// considers that an error rather than a choice.
	StateFailed State = "failed"
	// StateUnknown means the supervisor answered something unrecognised.
	// Deliberately distinct from stopped: reporting "stopped" for an answer
	// nobody parsed would be a confident lie.
	StateUnknown State = "unknown"
)

// Status is a service's current condition.
type Status struct {
	// Manager names the supervisor, for a support answer.
	Manager string `json:"manager"`
	State   State  `json:"state"`
	// Enabled means it starts at boot. Separate from running: a service can be
	// up and not enabled (it will not survive a reboot), which is exactly the
	// case an operator needs to be told about.
	Enabled bool `json:"enabled"`
	// Path is the unit or plist file.
	Path string `json:"path"`
	// Detail is the supervisor's own words, for when the state above is not
	// enough. Never parsed by anything.
	Detail string `json:"detail,omitempty"`
}

// Config is what an install needs to know.
type Config struct {
	// Binary is the absolute path to the tumika executable the supervisor
	// runs. Absolute because a supervisor has no useful working directory and
	// no PATH worth trusting.
	Binary string
	// Home is TUMIKA_HOME for the service. Set explicitly in the unit rather
	// than left to detection: a supervised process has no reliable HOME, and a
	// daemon that silently picked a different data directory after an upgrade
	// would look like total data loss.
	Home string
	// User is the account to run as. Linux only; ignored on macOS, where a
	// LaunchAgent runs as whoever loaded it and must, for the Keychain.
	User string
	// SealedKey is the systemd-creds blob to hand the service, or "" for none.
	//
	// When set, the unit gets LoadCredentialEncrypted= and systemd decrypts it
	// during startup — while still privileged — into a tmpfs the service
	// account can read. That indirection is the whole reason the backend works:
	// the daemon is unprivileged and could never read the host key itself.
	SealedKey string
}

// Errors callers distinguish.
var (
	// ErrUnsupportedPlatform means there is no service manager for this OS.
	ErrUnsupportedPlatform = errors.New("no service manager for this platform")
	// ErrNotInstalled means the operation needs a service that is not there.
	ErrNotInstalled = errors.New("tumika is not installed as a service")
	// ErrPrivilegesRequired means the caller is not permitted to do this.
	// Separated from a generic failure because the fix is a single word — sudo —
	// and an operator should not have to infer it from an EACCES.
	ErrPrivilegesRequired = errors.New("this needs elevated privileges")
)

// Manager installs and supervises tumika as a service.
type Manager interface {
	// Prepare makes the things an install depends on exist, without installing
	// anything: on Linux, the service account.
	//
	// Separate from Install because the caller has work to do BETWEEN the two —
	// key custody is probed by running a transient unit AS the service account,
	// so that account has to exist first. Folding it into Install made every
	// first install probe a user that did not exist yet, report no handover, and
	// drop to the file key permanently.
	//
	// Idempotent, and a no-op where there is nothing to prepare.
	Prepare(ctx context.Context, cfg Config) error
	// Install writes the unit and enables it. Idempotent: installing over an
	// existing service rewrites the unit and reloads, which is what an upgrade
	// needs.
	Install(ctx context.Context, cfg Config) error
	// Uninstall stops and removes the service. It leaves tumika's DATA alone —
	// removing a service must never be a way to lose credentials by accident.
	Uninstall(ctx context.Context) error
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	Status(ctx context.Context) (Status, error)
}

// New returns the manager for this platform.
func New() (Manager, error) {
	switch runtime.GOOS {
	case "darwin":
		return NewLaunchd()
	case "linux":
		return NewSystemd()
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedPlatform, runtime.GOOS)
	}
}

// Validate checks a config before anything is written.
//
// Up front, because a half-installed service is worse than a refused one: the
// unit would exist, the supervisor would try to run it, and the failure would
// arrive in a journal rather than in the terminal of the person who just typed
// the command.
func (c Config) Validate() error {
	switch {
	case c.Binary == "":
		return errors.New("no binary path")
	case !filepath.IsAbs(c.Binary):
		return fmt.Errorf("binary path %q is not absolute; a supervisor has no working directory to resolve it against", c.Binary)
	case c.Home == "":
		return errors.New("no home directory")
	case !filepath.IsAbs(c.Home):
		return fmt.Errorf("home %q is not absolute", c.Home)
	}

	// A newline would let a path append a directive of its own choosing to a
	// unit file that runs as root, or break out of a plist string.
	//
	// Whitespace and `%` are checked separately, by the systemd driver only:
	// they are unit-file grammar, and macOS's own default home is
	// ~/Library/Application Support/tumika — a perfectly valid path with a space
	// in it that a plist handles without complaint. Rejecting it here would
	// break every Mac install to protect Linux.
	for _, field := range []struct{ name, value string }{
		{"binary path", c.Binary},
		{"home", c.Home},
		{"user", c.User},
		{"sealed key path", c.SealedKey},
	} {
		if strings.ContainsAny(field.value, "\n\r") {
			return fmt.Errorf("%s contains a line break", field.name)
		}
	}

	if c.SealedKey != "" && !filepath.IsAbs(c.SealedKey) {
		return fmt.Errorf("sealed key path %q is not absolute", c.SealedKey)
	}
	if c.User != "" && !validUserName(c.User) {
		return fmt.Errorf("user %q is not a valid account name", c.User)
	}
	return nil
}

// validUserName is deliberately stricter than the platforms are.
//
// The name reaches `useradd` and a unit's User= directive, so the cost of
// permitting something exotic is an argument that means something else. Nobody
// needs a service account with a space in it.
func validUserName(name string) bool {
	if len(name) > 32 {
		return false
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9' && i > 0:
		case r == '_', r == '-' && i > 0:
		default:
			return false
		}
	}
	return name != ""
}

// runner executes a command and returns its combined output.
//
// An indirection so tests can drive the managers without a supervisor. Combined
// rather than stdout alone because systemctl and launchctl both explain
// themselves on stderr, and an error that says only "exit status 1" is an error
// an operator cannot act on.
type runner func(ctx context.Context, name string, args ...string) ([]byte, error)

func execRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	// Both scanners flag the variable command. The audit: every call site in
	// this package passes a literal — "systemctl", "launchctl", "useradd" — and
	// arguments built from package constants plus a Config that Validate has
	// already restricted (absolute paths, no line breaks, account names limited
	// to [a-z0-9_-]). Nothing reaching here comes from an HTTP request; the
	// service manager is only driven by a local CLI command.
	//
	// nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
	cmd := exec.CommandContext(ctx, name, args...) // #nosec G204 -- see the audit above: literal command names, args from a validated Config
	return cmd.CombinedOutput()
}

// Warnf receives non-fatal problems worth telling an operator about.
//
// A package-level sink rather than a returned error, because these are cases
// where the operation SUCCEEDED and something alongside it did not — a stop that
// timed out during an uninstall that still removed the unit. Returning an error
// would make the caller treat a completed removal as a failure; discarding it
// silently is how a surviving daemon process goes unnoticed.
var Warnf = func(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "warning: "+format+"\n", args...)
}

// detail renders a command's output for a warning, bounded so a runaway
// supervisor cannot fill a terminal.
func detail(out []byte) string {
	text := strings.TrimSpace(string(out))
	if text == "" {
		return ""
	}
	const max = 200
	if len(text) > max {
		text = text[:max] + "…"
	}
	return ": " + text
}

// commandError renders a failed supervisor call with its own output.
func commandError(what string, out []byte, err error) error {
	detail := strings.TrimSpace(string(out))
	if detail == "" {
		return fmt.Errorf("%s: %w", what, err)
	}
	return fmt.Errorf("%s: %w: %s", what, err, detail)
}
