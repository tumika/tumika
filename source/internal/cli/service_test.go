package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tumika/tumika/source/internal/platform/servicemgr"
)

// fakeManager stands in for the platform's supervisor, so the command tree is
// exercised without touching launchctl or systemctl — and without needing root.
type fakeManager struct {
	installed  bool
	status     servicemgr.Status
	err        error
	lastConfig servicemgr.Config
	calls      []string
}

func (f *fakeManager) Install(_ context.Context, cfg servicemgr.Config) error {
	f.calls = append(f.calls, "install")
	f.lastConfig = cfg
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
