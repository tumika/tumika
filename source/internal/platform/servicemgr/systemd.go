package servicemgr

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
)

// systemdUnitDir is where a system unit lives.
const systemdUnitDir = "/etc/systemd/system"

// credentialName is what systemd calls the master key. It matches
// secrets.CredentialName, and is repeated rather than imported because platform
// packages do not depend on one another.
const credentialName = "tumika-master-key" // #nosec G101 -- the NAME systemd binds a credential to, written into a world-readable unit file; not a secret

// Systemd supervises tumika with a system unit.
//
// A SYSTEM unit, not `systemd --user`: the Pi has no interactive login, and a
// user manager without `loginctl enable-linger` stops when the session ends —
// which for a machine nobody logs into means "never starts". The cost is that
// install needs root.
type Systemd struct {
	unitDir string
	run     runner
	// lookupUser reports whether an account exists. Injected so the
	// account-creation path is testable without touching /etc/passwd.
	lookupUser func(name string) bool
	// euid is the effective user id, so the privilege check is assertable.
	euid int
	// lookupIDs resolves an account to its uid and gid. Injected so the
	// ownership step is testable without creating a real account.
	lookupIDs func(name string) (account, error)
	// chown changes ownership. Injected because a test cannot give a file away
	// to another account without being root — and asserting on the CALLS is
	// what the test is actually about.
	chown func(path string, uid, gid int) error
}

// SystemdOption configures the manager, for tests.
type SystemdOption func(*Systemd)

func withSystemdUnitDir(dir string) SystemdOption {
	return func(s *Systemd) { s.unitDir = dir }
}

func withSystemdRunner(r runner) SystemdOption {
	return func(s *Systemd) { s.run = r }
}

func withSystemdUserLookup(f func(string) bool) SystemdOption {
	return func(s *Systemd) { s.lookupUser = f }
}

func withSystemdEUID(uid int) SystemdOption {
	return func(s *Systemd) { s.euid = uid }
}

func withSystemdIDLookup(f func(string) (account, error)) SystemdOption {
	return func(s *Systemd) { s.lookupIDs = f }
}

func withSystemdChown(f func(string, int, int) error) SystemdOption {
	return func(s *Systemd) { s.chown = f }
}

// NewSystemd builds the Linux manager.
func NewSystemd(opts ...SystemdOption) (*Systemd, error) {
	s := &Systemd{
		unitDir: systemdUnitDir,
		run:     execRunner,
		lookupUser: func(name string) bool {
			// `id` rather than parsing /etc/passwd, so NSS-backed directories —
			// LDAP, SSSD — answer for themselves. A machine that gets its
			// accounts from a directory would otherwise have the account
			// recreated locally on every install.
			// name is a Config.User that Validate already restricted to
			// [a-z0-9_-], and `--` stops anything that slipped through being
			// read as an option.
			return exec.Command("id", "--", name).Run() == nil // #nosec G204 -- name is validated by Config.Validate and guarded by --
		},
		euid: os.Geteuid(),
		// Lchown, not Chown: a symlink in the tree must not redirect the
		// ownership change onto whatever it points at.
		chown: os.Lchown,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}

func (s *Systemd) unitPath() string { return filepath.Join(s.unitDir, UnitName) }

// Install writes the unit, creates the service account, and enables it.
//
// Idempotent by construction: every step is either a rewrite or is skipped when
// already true, because the realistic second run of this command is an upgrade
// rather than a mistake.
func (s *Systemd) Install(ctx context.Context, cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	if cfg.User == "" {
		cfg.User = DefaultUser
	}
	if s.euid != 0 {
		return fmt.Errorf("%w: writing %s and creating the %s account both need root; try sudo",
			ErrPrivilegesRequired, s.unitPath(), cfg.User)
	}

	if err := s.ensureUser(ctx, cfg.User); err != nil {
		return err
	}
	if err := s.giveHomeToService(cfg); err != nil {
		return err
	}

	unit := renderUnit(cfg)
	// 0755 is the correct mode for /etc/systemd/system and gosec's 0750 advice
	// is wrong here: the directory is meant to be traversable and listable by
	// every tool on the box, and it holds no secret. Almost always a no-op,
	// since the directory ships with the distribution.
	if err := os.MkdirAll(s.unitDir, 0o755); err != nil { // #nosec G301 -- /etc/systemd/system is world-traversable by design and holds no secret
		return fmt.Errorf("create %s: %w", s.unitDir, err)
	}
	// 0644: systemd reads this as root, and an operator reading it is how they
	// find out what the service does. There is nothing secret in it — which is
	// exactly why the unit sets TUMIKA_HOME rather than any credential.
	if err := os.WriteFile(s.unitPath(), []byte(unit), 0o644); err != nil { // #nosec G306 -- a unit file is world-readable by design and holds no secret
		return fmt.Errorf("write %s: %w", s.unitPath(), err)
	}

	if out, err := s.run(ctx, "systemctl", "daemon-reload"); err != nil {
		return commandError("systemctl daemon-reload", out, err)
	}
	// enable --now, so one command leaves the service both running and
	// surviving a reboot. Enabling without starting is the failure an operator
	// discovers by rebooting; starting without enabling is the one they
	// discover months later.
	if out, err := s.run(ctx, "systemctl", "enable", "--now", UnitName); err != nil {
		return commandError("systemctl enable --now "+UnitName, out, err)
	}
	return nil
}

// ensureUser creates the service account if it is missing.
//
// A system account with no login shell and no home of its own: tumika's data
// lives under TUMIKA_HOME, which the installer owns, and an account that can log
// in is an account that can be logged into.
func (s *Systemd) ensureUser(ctx context.Context, name string) error {
	if s.lookupUser(name) {
		return nil
	}

	out, err := s.run(ctx, "useradd",
		"--system",
		"--no-create-home",
		"--shell", "/usr/sbin/nologin",
		"--comment", "tumika daemon",
		"--", name)
	if err != nil {
		return commandError("create the "+name+" account", out, err)
	}
	return nil
}

// giveHomeToService hands TUMIKA_HOME to the account the unit runs as.
//
// Without this the install SUCCEEDS and the service never starts. The layout is
// created 0700 by whoever ran the installer — root — and the unit runs as an
// unprivileged account, which cannot so much as traverse into the directory
// holding its own binary. systemd reports 203/EXEC "Permission denied" and
// Restart=always turns it into a loop, so `systemctl is-active` says
// "activating" forever rather than "failed".
//
// Recursive, because everything the daemon owns lives under here: the database
// it writes, the vendored provider binaries it executes, the sealed key it
// opens. Ownership changes; the 0700 modes do not, so the tree stays private to
// the service rather than becoming world-readable.
func (s *Systemd) giveHomeToService(cfg Config) error {
	account, err := s.resolveUser(cfg.User)
	if err != nil {
		return err
	}

	// The home may not exist yet on a first install — the daemon creates it on
	// first run. Create it here instead, owned correctly from the start,
	// because a daemon that cannot write its own parent directory cannot create
	// it either.
	if err := os.MkdirAll(cfg.Home, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", cfg.Home, err)
	}

	err = filepath.WalkDir(cfg.Home, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := s.chown(path, account.uid, account.gid); err != nil {
			return fmt.Errorf("give %s to %s: %w", path, cfg.User, err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	return nil
}

type account struct{ uid, gid int }

// resolveUser looks the account up by name, after it has been created.
func (s *Systemd) resolveUser(name string) (account, error) {
	if s.lookupIDs != nil {
		return s.lookupIDs(name)
	}

	u, err := user.Lookup(name)
	if err != nil {
		return account{}, fmt.Errorf("look up the %s account: %w", name, err)
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return account{}, fmt.Errorf("the %s account has a non-numeric uid %q", name, u.Uid)
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return account{}, fmt.Errorf("the %s account has a non-numeric gid %q", name, u.Gid)
	}
	return account{uid: uid, gid: gid}, nil
}

// Uninstall stops and removes the service, and nothing else.
//
// The account and the data are LEFT. Removing a service is a routine part of an
// upgrade or a reinstall, and making it also destroy every sealed credential
// would turn a reversible action into an irreversible one. Saying so is part of
// the command's output rather than a footnote.
func (s *Systemd) Uninstall(ctx context.Context) error {
	if _, err := os.Stat(s.unitPath()); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: no unit at %s", ErrNotInstalled, s.unitPath())
	}
	if s.euid != 0 {
		return fmt.Errorf("%w: removing %s needs root; try sudo", ErrPrivilegesRequired, s.unitPath())
	}

	// Failures here are reported but not fatal: the unit is being deleted, and
	// refusing to remove the file because the service was already stopped would
	// leave the machine in the half state this is meant to clear.
	if out, err := s.run(ctx, "systemctl", "disable", "--now", UnitName); err != nil {
		_ = out
	}

	if err := os.Remove(s.unitPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", s.unitPath(), err)
	}
	if out, err := s.run(ctx, "systemctl", "daemon-reload"); err != nil {
		return commandError("systemctl daemon-reload", out, err)
	}
	return nil
}

func (s *Systemd) Start(ctx context.Context) error {
	if err := s.requireInstalled(); err != nil {
		return err
	}
	if out, err := s.run(ctx, "systemctl", "start", UnitName); err != nil {
		return commandError("systemctl start "+UnitName, out, err)
	}
	return nil
}

func (s *Systemd) Stop(ctx context.Context) error {
	if err := s.requireInstalled(); err != nil {
		return err
	}
	if out, err := s.run(ctx, "systemctl", "stop", UnitName); err != nil {
		return commandError("systemctl stop "+UnitName, out, err)
	}
	return nil
}

func (s *Systemd) requireInstalled() error {
	if _, err := os.Stat(s.unitPath()); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: no unit at %s", ErrNotInstalled, s.unitPath())
	}
	return nil
}

// Status asks systemd rather than inferring.
//
// systemctl exits non-zero for "inactive" and "disabled", which are ANSWERS
// rather than failures — so the output is read and the exit code is not. Keying
// on the exit code reports every stopped service as an error.
func (s *Systemd) Status(ctx context.Context) (Status, error) {
	status := Status{Manager: "systemd", Path: s.unitPath(), State: StateNotInstalled}

	if _, err := os.Stat(s.unitPath()); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return status, nil
		}
		return status, fmt.Errorf("stat %s: %w", s.unitPath(), err)
	}

	active, _ := s.run(ctx, "systemctl", "is-active", UnitName)
	status.Detail = strings.TrimSpace(string(active))
	switch status.Detail {
	case "active", "activating", "reloading":
		status.State = StateRunning
	case "inactive", "deactivating":
		status.State = StateStopped
	case "failed":
		status.State = StateFailed
	default:
		status.State = StateUnknown
	}

	enabled, _ := s.run(ctx, "systemctl", "is-enabled", UnitName)
	status.Enabled = strings.TrimSpace(string(enabled)) == "enabled"

	return status, nil
}

// renderUnit builds the unit file.
//
// Written as a constant with substitutions rather than text/template: it is
// twenty lines, every value is validated before it gets here, and a template
// would add an error path for a rendering failure that cannot happen.
func renderUnit(cfg Config) string {
	var b strings.Builder

	b.WriteString("# Managed by tumika. Rewritten by `tumika install`; edit with care.\n")
	b.WriteString("[Unit]\n")
	b.WriteString("Description=tumika personal assistant daemon\n")
	b.WriteString("Documentation=https://github.com/tumika/tumika\n")
	// network-online, not network: the daemon reaches api.anthropic.com and a
	// bare network.target is satisfied long before a route exists. On a Pi
	// booting from cold that difference is a failed first start.
	b.WriteString("After=network-online.target\n")
	b.WriteString("Wants=network-online.target\n\n")

	b.WriteString("[Service]\n")
	b.WriteString("Type=simple\n")
	fmt.Fprintf(&b, "User=%s\n", cfg.User)
	fmt.Fprintf(&b, "Environment=TUMIKA_HOME=%s\n", cfg.Home)
	if cfg.SealedKey != "" {
		// systemd decrypts this during startup, as root, and hands the
		// plaintext to the service in $CREDENTIALS_DIRECTORY. The daemon never
		// runs systemd-creds — it could not, being unprivileged.
		fmt.Fprintf(&b, "LoadCredentialEncrypted=%s:%s\n", credentialName, cfg.SealedKey)
	}
	fmt.Fprintf(&b, "ExecStart=%s serve\n", cfg.Binary)
	// always, not on-failure: an update replaces the binary and exits ZERO to
	// be relaunched on the new one (ADR-0003). on-failure would leave the
	// machine with a stopped daemon and a successful-looking update.
	b.WriteString("Restart=always\n")
	b.WriteString("RestartSec=5\n")
	// Logs go to the journal, which is where an operator on a Pi will look.
	b.WriteString("StandardOutput=journal\n")
	b.WriteString("StandardError=journal\n\n")

	b.WriteString("# Hardening. Deliberately not ProtectSystem=strict or a ReadWritePaths\n")
	b.WriteString("# allowlist: the daemon rewrites its own binary under the home directory\n")
	b.WriteString("# during an update, and a stricter sandbox would make that fail at exactly\n")
	b.WriteString("# the moment nothing is watching.\n")
	b.WriteString("NoNewPrivileges=true\n")
	b.WriteString("PrivateTmp=true\n")
	b.WriteString("ProtectSystem=full\n")
	b.WriteString("ProtectHome=read-only\n")
	fmt.Fprintf(&b, "ReadWritePaths=%s\n", cfg.Home)
	b.WriteString("ProtectKernelTunables=true\n")
	b.WriteString("ProtectControlGroups=true\n")
	b.WriteString("RestrictSUIDSGID=true\n\n")

	b.WriteString("[Install]\n")
	b.WriteString("WantedBy=multi-user.target\n")

	return b.String()
}
