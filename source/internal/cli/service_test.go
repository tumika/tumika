package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tumika/tumika/source/internal/platform/paths"
	"github.com/tumika/tumika/source/internal/platform/secrets"
	"github.com/tumika/tumika/source/internal/platform/servicemgr"
)

// fakeManager stands in for the platform's supervisor, so the command tree is
// exercised without touching launchctl or systemctl — and without needing root.
type fakeManager struct {
	installed bool
	status    servicemgr.Status
	err       error
	// installErr fails only Install, so a test can reach the steps that run
	// between Prepare and Install — which is where the token is minted.
	installErr   error
	lastConfig   servicemgr.Config
	calls        []string
	preparedUser string
}

func (f *fakeManager) Prepare(_ context.Context, cfg servicemgr.Config) error {
	f.calls = append(f.calls, "prepare")
	f.preparedUser = cfg.User
	return f.err
}

func (f *fakeManager) Install(_ context.Context, cfg servicemgr.Config) error {
	f.calls = append(f.calls, "install")
	f.lastConfig = cfg
	if f.installErr != nil {
		return f.installErr
	}
	if f.err != nil {
		return f.err
	}
	f.installed = true
	return nil
}

func (f *fakeManager) Uninstall(context.Context) error {
	f.calls = append(f.calls, "uninstall")
	if f.err != nil {
		return f.err
	}
	f.installed = false
	return nil
}

func (f *fakeManager) Start(context.Context) error {
	f.calls = append(f.calls, "start")
	return f.err
}

func (f *fakeManager) Stop(context.Context) error {
	f.calls = append(f.calls, "stop")
	return f.err
}

func (f *fakeManager) Status(context.Context) (servicemgr.Status, error) {
	f.calls = append(f.calls, "status")
	return f.status, nil
}

// useFakeManager swaps the factory for the duration of a test.
func useFakeManager(t *testing.T, mgr *fakeManager) {
	t.Helper()

	original := managerFactory
	managerFactory = func() (servicemgr.Manager, error) { return mgr, nil }
	t.Cleanup(func() { managerFactory = original })
}

func TestStatusReportsAnUninstalledService(t *testing.T) {
	mgr := &fakeManager{status: servicemgr.Status{
		Manager: "systemd",
		State:   servicemgr.StateNotInstalled,
	}}
	useFakeManager(t, mgr)

	out, _, err := run(t, "status")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(out, "not_installed") {
		t.Errorf("output does not report the state: %s", out)
	}
	// An operator on a fresh machine should be told the next command, not left
	// to work it out.
	if !strings.Contains(out, "tumika install") {
		t.Errorf("output does not name the next step: %s", out)
	}
}

// A stopped service is a FACT, not a command failure: `tumika status` in a
// script should not need `|| true` to report one.
func TestStatusExitsZeroForAStoppedService(t *testing.T) {
	mgr := &fakeManager{status: servicemgr.Status{
		Manager: "systemd",
		State:   servicemgr.StateStopped,
		Path:    "/etc/systemd/system/tumika.service",
	}}
	useFakeManager(t, mgr)

	if _, _, err := run(t, "status"); err != nil {
		t.Errorf("status on a stopped service returned an error: %v", err)
	}
}

// Running but not enabled is the failure discovered months later, at the first
// reboot. Worth saying out loud.
func TestStatusWarnsWhenRunningButNotEnabled(t *testing.T) {
	mgr := &fakeManager{status: servicemgr.Status{
		Manager: "systemd",
		State:   servicemgr.StateRunning,
		Enabled: false,
		Path:    "/etc/systemd/system/tumika.service",
	}}
	useFakeManager(t, mgr)

	out, _, err := run(t, "status")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(out, "not come back") {
		t.Errorf("no warning that a reboot loses the service: %s", out)
	}
}

func TestStatusJSONIsParseable(t *testing.T) {
	mgr := &fakeManager{status: servicemgr.Status{
		Manager: "launchd",
		State:   servicemgr.StateRunning,
		Enabled: true,
		Path:    "/Users/someone/Library/LaunchAgents/com.tumika.daemon.plist",
	}}
	useFakeManager(t, mgr)

	out, _, err := run(t, "status", "--json")
	if err != nil {
		t.Fatalf("status --json: %v", err)
	}

	var parsed servicemgr.Status
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("output is not valid JSON (%v): %s", err, out)
	}
	if parsed.State != servicemgr.StateRunning || !parsed.Enabled {
		t.Errorf("round trip lost information: %+v", parsed)
	}
}

// Uninstall must say that the data survives. An operator reading only the
// command output should not have to guess whether their credentials are gone.
func TestUninstallSaysTheDataIsKept(t *testing.T) {
	mgr := &fakeManager{installed: true}
	useFakeManager(t, mgr)

	out, _, err := run(t, "uninstall")
	if err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if !strings.Contains(out, "untouched") {
		t.Errorf("output does not say the data survives: %s", out)
	}
}

func TestStartAndStopReachTheManager(t *testing.T) {
	for _, command := range []string{"start", "stop"} {
		mgr := &fakeManager{status: servicemgr.Status{Manager: "systemd", State: servicemgr.StateRunning}}
		useFakeManager(t, mgr)

		if _, _, err := run(t, command); err != nil {
			t.Fatalf("%s: %v", command, err)
		}
		if len(mgr.calls) == 0 || mgr.calls[0] != command {
			t.Errorf("%s did not reach the manager: %v", command, mgr.calls)
		}
	}
}

// A manager failure is reported rather than swallowed — and `tumika start` on a
// machine with no service must not look like success.
func TestAManagerFailureIsReported(t *testing.T) {
	mgr := &fakeManager{err: servicemgr.ErrNotInstalled}
	useFakeManager(t, mgr)

	_, _, err := run(t, "start")
	if !errors.Is(err, servicemgr.ErrNotInstalled) {
		t.Fatalf("= %v, want ErrNotInstalled", err)
	}
}

// The service runs a copy under the home directory, not whatever path the
// installer happened to be invoked from — which might be a download directory
// that is gone next week (ADR-0003).
func TestInstallCopiesTheBinaryIntoTheHome(t *testing.T) {
	home := t.TempDir()
	mgr := &fakeManager{status: servicemgr.Status{Manager: "systemd", State: servicemgr.StateRunning}}
	useFakeManager(t, mgr)

	// A stand-in for the running executable.
	source := filepath.Join(t.TempDir(), "tumika")
	if err := os.WriteFile(source, []byte("#!/bin/sh\n"), 0o755); err != nil { //nolint:gosec // a test fixture standing in for an executable
		t.Fatalf("write: %v", err)
	}

	target := filepath.Join(home, "bin", "tumika")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := copyExecutable(source, target); err != nil {
		t.Fatalf("copyExecutable: %v", err)
	}

	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("the binary was not copied: %v", err)
	}
	// 0755, not 0700: on Linux the unit runs as the service account while the
	// installer is root, so an owner-only binary is one the service cannot
	// execute — which is a 203/EXEC restart loop.
	if perm := info.Mode().Perm(); perm&0o111 == 0 {
		t.Errorf("mode = %#o, which is not executable", perm)
	}
}

// The copy is staged and renamed, because the destination may be the binary a
// running daemon is executing and truncating it kills the service mid-write.
func TestCopyingOverARunningBinaryIsAtomic(t *testing.T) {
	dir := t.TempDir()

	source := filepath.Join(dir, "new")
	if err := os.WriteFile(source, []byte("new binary"), 0o755); err != nil { //nolint:gosec // a test fixture standing in for an executable
		t.Fatalf("write: %v", err)
	}
	target := filepath.Join(dir, "live")
	if err := os.WriteFile(target, []byte("old binary"), 0o755); err != nil { //nolint:gosec // a test fixture standing in for an executable
		t.Fatalf("write: %v", err)
	}

	if err := copyExecutable(source, target); err != nil {
		t.Fatalf("copyExecutable: %v", err)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "new binary" {
		t.Errorf("target = %q, want the new binary", got)
	}

	// Nothing staged is left behind.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the directory: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".tumika.") {
			t.Errorf("a staging file was left behind: %s", entry.Name())
		}
	}
}

// useTestKeyCustody pins the master key, so nothing here reaches the real
// Keychain on the machine running the tests.
func useTestKeyCustody(t *testing.T) {
	t.Helper()
	t.Setenv("TUMIKA_MASTER_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
}

// A fresh install has to leave the operator with a token, or the daemon refuses
// to serve and Restart=always turns that refusal into a crash loop — an install
// that reports success and a service that never runs.
func TestInstallMintsAndPrintsATokenOnAFreshHost(t *testing.T) {
	useTestKeyCustody(t)
	home := t.TempDir()

	mgr := &fakeManager{status: servicemgr.Status{
		Manager: "systemd", State: servicemgr.StateRunning, Enabled: true,
	}}
	useFakeManager(t, mgr)

	out, _, err := run(t, "--home", home, "install")
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if !strings.Contains(out, "tmk_") {
		t.Errorf("no token was printed:\n%s", out)
	}
	// prepare first: the handover probe runs a transient unit AS the service
	// account, so that account has to exist before custody is decided.
	if len(mgr.calls) < 2 || mgr.calls[0] != "prepare" || mgr.calls[1] != "install" {
		t.Errorf("calls = %v, want prepare then install", mgr.calls)
	}
	if mgr.lastConfig.Home != home {
		t.Errorf("home = %q, want %q", mgr.lastConfig.Home, home)
	}
	if !filepath.IsAbs(mgr.lastConfig.Binary) {
		t.Errorf("binary = %q, want an absolute path", mgr.lastConfig.Binary)
	}
}

// A second install must not rotate the token: that would break every client the
// operator had already configured, on what is meant to be an upgrade.
func TestASecondInstallKeepsTheExistingToken(t *testing.T) {
	useTestKeyCustody(t)
	home := t.TempDir()

	mgr := &fakeManager{status: servicemgr.Status{Manager: "systemd", State: servicemgr.StateRunning}}
	useFakeManager(t, mgr)

	first, _, err := run(t, "--home", home, "install")
	if err != nil {
		t.Fatalf("first install: %v", err)
	}
	second, _, err := run(t, "--home", home, "install")
	if err != nil {
		t.Fatalf("second install: %v", err)
	}

	if !strings.Contains(first, "tmk_") {
		t.Fatalf("the first install printed no token:\n%s", first)
	}
	if strings.Contains(second, "tmk_") {
		t.Errorf("the second install rotated the token, breaking every configured client:\n%s", second)
	}
}

// Being able to SEAL a key and being able to RECEIVE one are different
// capabilities. Committing to systemd-creds without checking leaves a daemon
// that can never start — which is exactly what a container does.
func TestInstallFallsBackWhenTheHandoverDoesNotWork(t *testing.T) {
	useTestKeyCustody(t)
	home := t.TempDir()

	restore := installGOOS
	installGOOS = "linux"
	t.Cleanup(func() { installGOOS = restore })

	sealed := filepath.Join(home, "master.cred")
	credsUsableStub(t, true)
	sealStub(t, func(_ context.Context, path string, _ secretsRunner) error {
		return os.WriteFile(path, []byte("sealed"), 0o600)
	})
	handoverStub(t, false)

	mgr := &fakeManager{status: servicemgr.Status{Manager: "systemd", State: servicemgr.StateRunning}}
	useFakeManager(t, mgr)

	out, _, err := run(t, "--home", home, "install")
	if err != nil {
		t.Fatalf("install: %v", err)
	}

	if mgr.lastConfig.SealedKey != "" {
		t.Errorf("the unit was told to load a credential systemd will not deliver: %q",
			mgr.lastConfig.SealedKey)
	}
	// Nothing is sealed under it yet, so leaving it behind would make every
	// later start fail closed on a key nobody can use.
	if _, statErr := os.Stat(sealed); statErr == nil {
		t.Error("the unusable sealed key was left behind")
	}
	if !strings.Contains(out, "file-based key") {
		t.Errorf("the operator was not told which custody they ended up with:\n%s", out)
	}
}

// When the handover does work, the unit gets the directive — that indirection is
// the only reason the backend works at all.
func TestInstallCommitsToSystemdCredsWhenTheHandoverWorks(t *testing.T) {
	useTestKeyCustody(t)
	home := t.TempDir()

	restore := installGOOS
	installGOOS = "linux"
	t.Cleanup(func() { installGOOS = restore })

	credsUsableStub(t, true)
	sealStub(t, func(_ context.Context, path string, _ secretsRunner) error {
		return os.WriteFile(path, []byte("sealed"), 0o600)
	})
	handoverStub(t, true)

	mgr := &fakeManager{status: servicemgr.Status{Manager: "systemd", State: servicemgr.StateRunning}}
	useFakeManager(t, mgr)

	if _, _, err := run(t, "--home", home, "install"); err != nil {
		t.Fatalf("install: %v", err)
	}
	if mgr.lastConfig.SealedKey != filepath.Join(home, "master.cred") {
		t.Errorf("the unit does not hand the sealed key over: %q", mgr.lastConfig.SealedKey)
	}
}

// A daemon already running on a file key must not be moved onto systemd-creds:
// the keys are unrelated, and switching would make every stored credential
// unreadable while reporting a healthy start.
func TestInstallLeavesAnExistingFileKeyAlone(t *testing.T) {
	useTestKeyCustody(t)
	home := t.TempDir()

	restore := installGOOS
	installGOOS = "linux"
	t.Cleanup(func() { installGOOS = restore })

	if err := os.WriteFile(filepath.Join(home, "master.key"), []byte("existing"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	sealed := false
	credsUsableStub(t, true)
	sealStub(t, func(context.Context, string, secretsRunner) error {
		sealed = true
		return nil
	})
	handoverStub(t, true)

	mgr := &fakeManager{status: servicemgr.Status{Manager: "systemd", State: servicemgr.StateRunning}}
	useFakeManager(t, mgr)

	if _, _, err := run(t, "--home", home, "install"); err != nil {
		t.Fatalf("install: %v", err)
	}
	if sealed {
		t.Error("custody was switched under a daemon that already has credentials sealed under a file key")
	}
	if mgr.lastConfig.SealedKey != "" {
		t.Errorf("the unit was pointed at a sealed key that does not exist: %q", mgr.lastConfig.SealedKey)
	}
}

// macOS seals nothing: custody there is the Keychain.
func TestInstallSealsNothingOffLinux(t *testing.T) {
	useTestKeyCustody(t)
	home := t.TempDir()

	restore := installGOOS
	installGOOS = "darwin"
	t.Cleanup(func() { installGOOS = restore })

	credsUsableStub(t, true)
	sealStub(t, func(context.Context, string, secretsRunner) error {
		t.Error("a key was sealed with systemd-creds on macOS")
		return nil
	})

	mgr := &fakeManager{status: servicemgr.Status{Manager: "launchd", State: servicemgr.StateRunning}}
	useFakeManager(t, mgr)

	if _, _, err := run(t, "--home", home, "install"); err != nil {
		t.Fatalf("install: %v", err)
	}
	if mgr.lastConfig.SealedKey != "" {
		t.Errorf("SealedKey = %q on macOS", mgr.lastConfig.SealedKey)
	}
}

// The service runs a copy under the home directory, not the path the installer
// happened to be invoked from — which might be a download directory that is gone
// next week (ADR-0003).
func TestInstallResolvesTheBinaryIntoTheHome(t *testing.T) {
	useTestKeyCustody(t)
	home := t.TempDir()

	p, err := paths.Resolve(home)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	got, err := installedBinary(p)
	if err != nil {
		t.Fatalf("installedBinary: %v", err)
	}
	if got != filepath.Join(p.Bin, "tumika") {
		t.Errorf("binary = %q, want it under the home directory", got)
	}
	if _, err := os.Stat(got); err != nil {
		t.Errorf("the binary was not put in place: %v", err)
	}
}

// An explicit --binary is honoured, so an operator can point the unit at a
// packaged path they manage themselves.
func TestInstallHonoursAnExplicitBinary(t *testing.T) {
	useTestKeyCustody(t)
	home := t.TempDir()

	mgr := &fakeManager{status: servicemgr.Status{Manager: "systemd", State: servicemgr.StateRunning}}
	useFakeManager(t, mgr)

	if _, _, err := run(t, "--home", home, "install", "--binary", "/opt/tumika/bin/tumika"); err != nil {
		t.Fatalf("install: %v", err)
	}
	if mgr.lastConfig.Binary != "/opt/tumika/bin/tumika" {
		t.Errorf("binary = %q, want the explicit one", mgr.lastConfig.Binary)
	}
}

func TestInstallPassesTheUserThrough(t *testing.T) {
	useTestKeyCustody(t)
	home := t.TempDir()

	mgr := &fakeManager{status: servicemgr.Status{Manager: "systemd", State: servicemgr.StateRunning}}
	useFakeManager(t, mgr)

	if _, _, err := run(t, "--home", home, "install", "--user", "assistant"); err != nil {
		t.Fatalf("install: %v", err)
	}
	if mgr.lastConfig.User != "assistant" {
		t.Errorf("user = %q, want the one asked for", mgr.lastConfig.User)
	}
}

// secretsRunner mirrors secrets.CredsRunner, so the stubs below match the real
// signatures without the test importing it for a type alone.
type secretsRunner = secrets.CredsRunner

func credsUsableStub(t *testing.T, usable bool) {
	t.Helper()
	original := credsUsable
	credsUsable = func(context.Context, secrets.CredsRunner) bool { return usable }
	t.Cleanup(func() { credsUsable = original })
}

func handoverStub(t *testing.T, works bool) {
	t.Helper()
	original := handoverWorks
	handoverWorks = func(context.Context, string, string, secrets.HandoverProbe) bool { return works }
	t.Cleanup(func() { handoverWorks = original })
}

func sealStub(t *testing.T, fn func(context.Context, string, secrets.CredsRunner) error) {
	t.Helper()
	original := sealMasterKey
	sealMasterKey = fn
	t.Cleanup(func() { sealMasterKey = original })
}

// A system unit runs as an unprivileged account that cannot reach a per-user
// directory. `sudo tumika install` resolves ROOT's home, so without this the
// unit named /root/.local/state/tumika and the service died at 203/EXEC while
// the command printed "state running". The container hid it, because
// InContainer() forces the system path anyway.
func TestASystemInstallUsesTheSystemHome(t *testing.T) {
	useTestKeyCustody(t)
	t.Setenv(paths.HomeEnv, "")

	restore := installGOOS
	installGOOS = "linux"
	t.Cleanup(func() { installGOOS = restore })

	mgr := &fakeManager{status: servicemgr.Status{Manager: "systemd", State: servicemgr.StateRunning}}
	useFakeManager(t, mgr)

	// The real system home is not writable in a test, so only the DECISION is
	// checked — which is the part that was wrong.
	_, _, _ = run(t, "install")

	if mgr.lastConfig.Home != "" && mgr.lastConfig.Home != paths.SystemHome {
		t.Errorf("home = %q, want %q", mgr.lastConfig.Home, paths.SystemHome)
	}
	if strings.Contains(mgr.lastConfig.Home, ".local/state") {
		t.Errorf("home = %q, which the service account cannot reach", mgr.lastConfig.Home)
	}
}

// An explicit --home still wins: an operator who said where meant it.
func TestAnExplicitHomeBeatsTheSystemDefault(t *testing.T) {
	useTestKeyCustody(t)
	home := t.TempDir()

	restore := installGOOS
	installGOOS = "linux"
	t.Cleanup(func() { installGOOS = restore })

	mgr := &fakeManager{status: servicemgr.Status{Manager: "systemd", State: servicemgr.StateRunning}}
	useFakeManager(t, mgr)

	if _, _, err := run(t, "--home", home, "install"); err != nil {
		t.Fatalf("install: %v", err)
	}
	if mgr.lastConfig.Home != home {
		t.Errorf("home = %q, want the explicit %q", mgr.lastConfig.Home, home)
	}
}

// The handover is probed as the account the unit will ACTUALLY run as. Probing
// the default while the unit runs as someone else verifies delivery to an
// account that never runs the service.
func TestTheHandoverIsProbedAsTheConfiguredUser(t *testing.T) {
	useTestKeyCustody(t)
	home := t.TempDir()

	restore := installGOOS
	installGOOS = "linux"
	t.Cleanup(func() { installGOOS = restore })

	var probedAs string
	credsUsableStub(t, true)
	sealStub(t, func(_ context.Context, path string, _ secretsRunner) error {
		return os.WriteFile(path, []byte("sealed"), 0o600)
	})
	original := handoverWorks
	handoverWorks = func(_ context.Context, _, user string, _ secrets.HandoverProbe) bool {
		probedAs = user
		return true
	}
	t.Cleanup(func() { handoverWorks = original })

	mgr := &fakeManager{status: servicemgr.Status{Manager: "systemd", State: servicemgr.StateRunning}}
	useFakeManager(t, mgr)

	if _, _, err := run(t, "--home", home, "install", "--user", "assistant"); err != nil {
		t.Fatalf("install: %v", err)
	}
	if probedAs != "assistant" {
		t.Errorf("probed as %q, want the account the unit runs as", probedAs)
	}
	if mgr.preparedUser != "assistant" {
		t.Errorf("prepared %q, want the account that will be probed", mgr.preparedUser)
	}
}

// The token's hash is stored the moment it is minted, so an install that fails
// afterwards — the common "try sudo" path — must still have printed it. A re-run
// finds one configured and prints nothing, leaving a daemon with a token nobody
// has ever seen.
func TestTheTokenIsPrintedEvenIfTheInstallFails(t *testing.T) {
	useTestKeyCustody(t)
	home := t.TempDir()

	mgr := &fakeManager{installErr: servicemgr.ErrPrivilegesRequired}
	useFakeManager(t, mgr)

	out, _, err := run(t, "--home", home, "install")
	if err == nil {
		t.Fatal("a failing install reported success")
	}
	if !strings.Contains(out, "tmk_") {
		t.Errorf("the minted token was lost when the install failed:\n%s", out)
	}
}

// Starting is not running, and the operator is told what to do about it.
func TestStatusDoesNotCallAStartingServiceRunning(t *testing.T) {
	mgr := &fakeManager{status: servicemgr.Status{
		Manager: "systemd",
		State:   servicemgr.StateStarting,
		Enabled: true,
		Path:    "/etc/systemd/system/tumika.service",
	}}
	useFakeManager(t, mgr)

	out, _, err := run(t, "status")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if strings.Contains(out, "state   running") {
		t.Errorf("a starting service is reported as running:\n%s", out)
	}
	if !strings.Contains(out, "restarting in a loop") {
		t.Errorf("the operator is not told what a stuck 'starting' means:\n%s", out)
	}
}
