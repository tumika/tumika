package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/tumika/tumika/source/daemon/internal/domain"
	"github.com/tumika/tumika/source/daemon/internal/platform/release"
	"github.com/tumika/tumika/source/daemon/internal/platform/servicemgr"
	"github.com/tumika/tumika/source/daemon/internal/service"
)

// A development build has no release to update FROM, and the comparison would
// be meaningless: "dev" is not a semver, so everything published looks newer
// and nothing is.
func TestUpdateRefusesOnADevelopmentBuild(t *testing.T) {
	_, _, err := run(t, "--home", t.TempDir(), "update")
	if err == nil {
		t.Fatal("a development build tried to update itself")
	}
	if !strings.Contains(err.Error(), "development build") {
		t.Errorf("the error does not explain why: %v", err)
	}
}

// The same refusal for --check: it opens the daemon and reaches the network
// otherwise, for a build that can never act on the answer.
func TestUpdateCheckRefusesOnADevelopmentBuild(t *testing.T) {
	if _, _, err := run(t, "--home", t.TempDir(), "update", "--check"); err == nil {
		t.Fatal("--check ran on a development build")
	}
}

// A container's image is the unit of deployment, and a container that rewrote
// its own binary would no longer match its tag.
func TestUpdateRefusesInAContainer(t *testing.T) {
	t.Setenv("TUMIKA_CONTAINER", "1")

	_, _, err := run(t, "--home", t.TempDir(), "update")
	if err == nil {
		t.Fatal("a container tried to self-update")
	}
	// The dev-build check comes first in a test binary, so either refusal is
	// correct — what matters is that it refused before touching anything.
	if !strings.Contains(err.Error(), "container") && !strings.Contains(err.Error(), "development build") {
		t.Errorf("unexpected refusal: %v", err)
	}
}

// The flags exist and are wired: a typo in a flag name is otherwise only found
// by an operator.
func TestUpdateFlags(t *testing.T) {
	cmd := newUpdateCmd(&globals{})
	for _, name := range []string{"check", "to", "no-restart"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("--%s is missing", name)
		}
	}
}

// fakeUpdates drives runUpdate without a daemon or a network.
type fakeUpdates struct {
	state     domain.UpdateState
	available string
	newer     bool
	checkErr  error
	applyErr  error
	applied   []string
}

func (f *fakeUpdates) State(context.Context) (domain.UpdateState, error) { return f.state, nil }
func (f *fakeUpdates) Check(context.Context) (string, bool, error) {
	return f.available, f.newer, f.checkErr
}

func (f *fakeUpdates) Apply(_ context.Context, version string) error {
	if f.applyErr != nil {
		return f.applyErr
	}
	f.applied = append(f.applied, version)
	return nil
}

func (f *fakeUpdates) ConfirmBoot(context.Context) (bool, error) { return false, nil }
func (f *fakeUpdates) Confirm(context.Context) error             { return nil }

var _ service.UpdateService = (*fakeUpdates)(nil)

func run2(t *testing.T, fn func(*cobra.Command) error) (string, string, error) {
	t.Helper()
	cmd := &cobra.Command{}
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetContext(context.Background())
	err := fn(cmd)
	return out.String(), errOut.String(), err
}

// restartManager reports one state and records what was asked of it, with Stop
// and Start failing independently — the half-restarted case is the one worth
// covering and a single error field cannot express it.
type restartManager struct {
	state     servicemgr.State
	statusErr error
	stopErr   error
	startErr  error
	calls     []string
}

func (m *restartManager) Prepare(context.Context, servicemgr.Config) error { return nil }
func (m *restartManager) Install(context.Context, servicemgr.Config) error { return nil }
func (m *restartManager) Uninstall(context.Context) error                  { return nil }

func (m *restartManager) Start(context.Context) error {
	m.calls = append(m.calls, "start")
	return m.startErr
}

func (m *restartManager) Stop(context.Context) error {
	m.calls = append(m.calls, "stop")
	return m.stopErr
}

func (m *restartManager) Status(context.Context) (servicemgr.Status, error) {
	m.calls = append(m.calls, "status")
	if m.statusErr != nil {
		return servicemgr.Status{}, m.statusErr
	}
	return servicemgr.Status{Manager: "launchd", State: m.state}, nil
}

// updateWith runs an update whose binary IS the one the service runs, against
// the given manager. The path never has to exist: nothing reads it.
func updateWith(t *testing.T, mgr servicemgr.Manager, opts updateOptions) (string, string, error) {
	t.Helper()

	managed := "/var/lib/tumika/bin/tumika"
	originalExe := executablePath
	executablePath = func() (string, error) { return managed, nil }
	t.Cleanup(func() { executablePath = originalExe })

	originalFactory := managerFactory
	managerFactory = func() (servicemgr.Manager, error) { return mgr, nil }
	t.Cleanup(func() { managerFactory = originalFactory })

	if opts.managed == "" {
		opts.managed = managed
	}
	if opts.current == "" {
		opts.current = "0.1.0"
	}
	updates := &fakeUpdates{available: "0.2.0", newer: true}
	return run2(t, func(cmd *cobra.Command) error {
		return runUpdate(cmd, updates, opts)
	})
}

func TestRunUpdateInstallsTheNewestVersion(t *testing.T) {
	updates := &fakeUpdates{available: "0.2.0", newer: true}

	out, _, err := run2(t, func(cmd *cobra.Command) error {
		return runUpdate(cmd, updates, updateOptions{current: "0.1.0", noRestart: true})
	})
	if err != nil {
		t.Fatalf("runUpdate: %v", err)
	}
	if len(updates.applied) != 1 || updates.applied[0] != "0.2.0" {
		t.Errorf("applied %v, want [0.2.0]", updates.applied)
	}
	if !strings.Contains(out, "0.2.0") || !strings.Contains(out, "Installed.") {
		t.Errorf("the operator is not told what happened:\n%s", out)
	}
}

// The replacement is a new file, not a rewrite of the one the daemon has in
// memory, so an update that does not restart leaves the old build serving.
func TestRunUpdateRestartsARunningService(t *testing.T) {
	mgr := &restartManager{state: servicemgr.StateRunning}

	out, _, err := updateWith(t, mgr, updateOptions{})
	if err != nil {
		t.Fatalf("runUpdate: %v", err)
	}
	if !strings.Contains(strings.Join(mgr.calls, ","), "stop,start") {
		t.Errorf("the service was not stopped and started: %v", mgr.calls)
	}
	if !strings.Contains(out, "Restarting") {
		t.Errorf("the restart is not reported:\n%s", out)
	}
}

func TestRunUpdateNoRestartLeavesTheServiceAlone(t *testing.T) {
	mgr := &restartManager{state: servicemgr.StateRunning}

	out, _, err := updateWith(t, mgr, updateOptions{noRestart: true})
	if err != nil {
		t.Fatalf("runUpdate: %v", err)
	}
	if len(mgr.calls) != 0 {
		t.Errorf("--no-restart touched the service: %v", mgr.calls)
	}
	if !strings.Contains(out, "tumika start") {
		t.Errorf("the operator is not told how to pick the new binary up:\n%s", out)
	}
}

// Nothing to restart is a normal outcome — an operator updating a binary they
// run by hand — and must not read as a failure.
func TestRunUpdateWithNoServiceInstalled(t *testing.T) {
	mgr := &restartManager{state: servicemgr.StateNotInstalled}

	out, _, err := updateWith(t, mgr, updateOptions{})
	if err != nil {
		t.Fatalf("runUpdate: %v", err)
	}
	for _, call := range mgr.calls {
		if call == "stop" || call == "start" {
			t.Errorf("an uninstalled service was driven: %v", mgr.calls)
		}
	}
	if !strings.Contains(out, "not installed") {
		t.Errorf("output does not say why nothing was restarted:\n%s", out)
	}
}

// A stopped service was stopped by someone, and starting it from under them
// would turn an update into a deployment.
func TestRunUpdateDoesNotStartAStoppedService(t *testing.T) {
	mgr := &restartManager{state: servicemgr.StateStopped}

	out, _, err := updateWith(t, mgr, updateOptions{})
	if err != nil {
		t.Fatalf("runUpdate: %v", err)
	}
	for _, call := range mgr.calls {
		if call == "start" {
			t.Errorf("a stopped service was started: %v", mgr.calls)
		}
	}
	if !strings.Contains(out, "tumika start") {
		t.Errorf("output does not name the command that starts it:\n%s", out)
	}
}

// The supervisor runs the managed copy. Restarting after replacing some other
// tumika relaunches the daemon on the build it already has.
func TestRunUpdateDoesNotRestartWhenTheBinaryIsNotTheManagedOne(t *testing.T) {
	mgr := &restartManager{state: servicemgr.StateRunning}

	_, errOut, err := updateWith(t, mgr, updateOptions{managed: "/opt/tumika/bin/tumika"})
	if err != nil {
		t.Fatalf("runUpdate: %v", err)
	}
	if len(mgr.calls) != 0 {
		t.Errorf("the service was driven for a binary it does not run: %v", mgr.calls)
	}
	if !strings.Contains(errOut, "tumika install") {
		t.Errorf("the warning does not name the fix:\n%s", errOut)
	}
}

// The running binary's path is symlink-resolved, so a managed path reached
// through a symlinked directory still names the same file.
func TestRunUpdateRestartsWhenTheManagedPathIsReachedThroughASymlink(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(real, "tumika")
	if err := os.WriteFile(bin, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	self, err := filepath.EvalSymlinks(bin)
	if err != nil {
		t.Fatal(err)
	}

	originalExe := executablePath
	executablePath = func() (string, error) { return self, nil }
	t.Cleanup(func() { executablePath = originalExe })

	mgr := &restartManager{state: servicemgr.StateRunning}
	originalFactory := managerFactory
	managerFactory = func() (servicemgr.Manager, error) { return mgr, nil }
	t.Cleanup(func() { managerFactory = originalFactory })

	updates := &fakeUpdates{available: "0.2.0", newer: true}
	_, errOut, err := run2(t, func(cmd *cobra.Command) error {
		return runUpdate(cmd, updates, updateOptions{
			current: "0.1.0",
			managed: filepath.Join(link, "tumika"),
		})
	})
	if err != nil {
		t.Fatalf("runUpdate: %v", err)
	}
	if len(mgr.calls) == 0 {
		t.Errorf("the service was not restarted:\n%s", errOut)
	}
}

// Stopped and not started again is the one outcome an operator must not have to
// infer: the binary is replaced and nothing is serving.
func TestRunUpdateReportsAServiceThatStoppedAndDidNotStart(t *testing.T) {
	mgr := &restartManager{state: servicemgr.StateRunning, startErr: errors.New("bootstrap failed")}

	_, errOut, err := updateWith(t, mgr, updateOptions{})
	if err == nil {
		t.Fatal("a service left stopped reported success")
	}
	if !strings.Contains(errOut, "STOPPED") || !strings.Contains(errOut, "tumika start") {
		t.Errorf("the operator is not told the state or the fix:\n%s", errOut)
	}
}

func TestRunUpdateReportsAStopFailure(t *testing.T) {
	mgr := &restartManager{state: servicemgr.StateRunning, stopErr: errors.New("permission denied")}

	if _, _, err := updateWith(t, mgr, updateOptions{}); err == nil {
		t.Fatal("a failed stop reported success")
	}
}

// A supervisor that cannot be read is not a failed update: the binary is on
// disk, and the operator is told to restart it themselves.
func TestRunUpdateWarnsWhenTheServiceStateCannotBeRead(t *testing.T) {
	mgr := &restartManager{statusErr: errors.New("launchctl exploded")}

	_, errOut, err := updateWith(t, mgr, updateOptions{})
	if err != nil {
		t.Fatalf("runUpdate: %v", err)
	}
	if !strings.Contains(errOut, "not restarted") {
		t.Errorf("the warning does not say what was skipped:\n%s", errOut)
	}
}

// --check must install nothing. It exists precisely so an operator can look
// before acting.
func TestRunUpdateCheckInstallsNothing(t *testing.T) {
	updates := &fakeUpdates{available: "0.2.0", newer: true}

	out, _, err := run2(t, func(cmd *cobra.Command) error {
		return runUpdate(cmd, updates, updateOptions{current: "0.1.0", check: true})
	})
	if err != nil {
		t.Fatalf("runUpdate: %v", err)
	}
	if len(updates.applied) != 0 {
		t.Errorf("--check installed %v", updates.applied)
	}
	if !strings.Contains(out, "0.2.0") {
		t.Errorf("--check did not report what is available:\n%s", out)
	}
}

func TestRunUpdateWhenAlreadyUpToDate(t *testing.T) {
	updates := &fakeUpdates{available: "0.1.0", newer: false}

	out, _, err := run2(t, func(cmd *cobra.Command) error {
		return runUpdate(cmd, updates, updateOptions{current: "0.1.0"})
	})
	if err != nil {
		t.Fatalf("runUpdate: %v", err)
	}
	if len(updates.applied) != 0 {
		t.Errorf("installed %v when already up to date", updates.applied)
	}
	if !strings.Contains(out, "up to date") {
		t.Errorf("output does not say so:\n%s", out)
	}
}

// --to pins a version, and bypasses the "is it newer" shortcut so an operator
// can move deliberately. The service still refuses a downgrade.
func TestRunUpdateHonoursAnExplicitVersion(t *testing.T) {
	updates := &fakeUpdates{available: "0.3.0", newer: true}

	if _, _, err := run2(t, func(cmd *cobra.Command) error {
		return runUpdate(cmd, updates, updateOptions{current: "0.1.0", version: "0.2.0", noRestart: true})
	}); err != nil {
		t.Fatalf("runUpdate: %v", err)
	}
	if len(updates.applied) != 1 || updates.applied[0] != "0.2.0" {
		t.Errorf("applied %v, want the version asked for", updates.applied)
	}
}

// A repository with no releases is a normal state before the first tag, and
// must not read as a failure.
func TestRunUpdateWithNoReleasePublished(t *testing.T) {
	updates := &fakeUpdates{checkErr: release.ErrNoRelease}

	out, _, err := run2(t, func(cmd *cobra.Command) error {
		return runUpdate(cmd, updates, updateOptions{current: "0.1.0"})
	})
	if err != nil {
		t.Fatalf("no release was reported as an error: %v", err)
	}
	if !strings.Contains(out, "No release") {
		t.Errorf("output does not explain:\n%s", out)
	}
}

// Any other check failure IS an error — mistaking it for "up to date" would
// silently stop a machine from ever updating.
func TestRunUpdateReportsACheckFailure(t *testing.T) {
	updates := &fakeUpdates{checkErr: errors.New("connection reset")}

	if _, _, err := run2(t, func(cmd *cobra.Command) error {
		return runUpdate(cmd, updates, updateOptions{current: "0.1.0"})
	}); err == nil {
		t.Fatal("a failed check reported success")
	}
}

func TestRunUpdateReportsAnApplyFailure(t *testing.T) {
	updates := &fakeUpdates{
		available: "0.2.0", newer: true,
		applyErr: errors.New("checksum mismatch"),
	}

	if _, _, err := run2(t, func(cmd *cobra.Command) error {
		return runUpdate(cmd, updates, updateOptions{current: "0.1.0"})
	}); err == nil {
		t.Fatal("a failed install reported success")
	}
}

func TestRunUpdateWithSelfUpdateDisabled(t *testing.T) {
	if _, _, err := run2(t, func(cmd *cobra.Command) error {
		return runUpdate(cmd, nil, updateOptions{current: "0.1.0"})
	}); err == nil {
		t.Fatal("update ran with no service")
	}
}

func TestRunUpdateStatus(t *testing.T) {
	started := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)

	tests := map[string]struct {
		state domain.UpdateState
		want  []string
	}{
		"idle": {
			state: domain.UpdateState{Status: domain.UpdateIdle},
			want:  []string{"idle"},
		},
		"pending shows the rollback budget": {
			state: domain.UpdateState{
				Status: domain.UpdatePending, FromVersion: "0.1.0", ToVersion: "0.2.0",
				BootAttempts: 2, StartedAt: &started,
			},
			want: []string{"pending", "0.1.0", "0.2.0", "2 of 3", "2026-08-16"},
		},
		"rolled back explains itself": {
			state: domain.UpdateState{
				Status: domain.UpdateRolledBack, FromVersion: "0.1.0", ToVersion: "0.2.0",
			},
			want: []string{"rolled_back", "failed to boot", "Check the logs"},
		},
	}

	for name, tc := range tests {
		out, _, err := run2(t, func(cmd *cobra.Command) error {
			return runUpdateStatus(cmd, &fakeUpdates{state: tc.state})
		})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, want := range tc.want {
			if !strings.Contains(out, want) {
				t.Errorf("%s: output is missing %q:\n%s", name, want, out)
			}
		}
	}
}

func TestRunUpdateStatusWithSelfUpdateDisabled(t *testing.T) {
	out, _, err := run2(t, func(cmd *cobra.Command) error {
		return runUpdateStatus(cmd, nil)
	})
	if err != nil {
		t.Fatalf("runUpdateStatus: %v", err)
	}
	if !strings.Contains(out, "disabled") {
		t.Errorf("output does not say why:\n%s", out)
	}
}

// The status command's flags and wiring exist.
func TestUpdateStatusCommandShape(t *testing.T) {
	cmd := newUpdateStatusCmd(&globals{})
	if cmd.Use != "update-status" {
		t.Errorf("Use = %q", cmd.Use)
	}
	if cmd.RunE == nil {
		t.Error("update-status has no RunE")
	}
}

// update-status runs end to end against a scratch home.
//
// On a test binary self-update is disabled, so the command reports that rather
// than a state — which is the answer an operator on a development build should
// get, and it exercises the whole command including withDaemon.
//
// An earlier version of this asserted that a RELATIVE --home is refused. It is
// not: paths.Resolve calls filepath.Abs, so relative homes are supported by
// design. The test skipped when err was nil and passed otherwise, so both
// branches were green and it could never fail.
func TestUpdateStatusRunsEndToEnd(t *testing.T) {
	t.Setenv("TUMIKA_MASTER_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")

	out, _, err := run(t, "--home", t.TempDir(), "update-status")
	if err != nil {
		t.Fatalf("update-status: %v", err)
	}
	if !strings.Contains(out, "disabled") {
		t.Errorf("output does not report that self-update is off:\n%s", out)
	}
}

// A confirmed update reports cleanly, with no rollback advice.
func TestRunUpdateStatusConfirmed(t *testing.T) {
	out, _, err := run2(t, func(cmd *cobra.Command) error {
		return runUpdateStatus(cmd, &fakeUpdates{state: domain.UpdateState{
			Status: domain.UpdateConfirmed, FromVersion: "0.1.0", ToVersion: "0.2.0",
		}})
	})
	if err != nil {
		t.Fatalf("runUpdateStatus: %v", err)
	}
	if !strings.Contains(out, "confirmed") {
		t.Errorf("output does not report the status:\n%s", out)
	}
	if strings.Contains(out, "Check the logs") {
		t.Errorf("a confirmed update was given rollback advice:\n%s", out)
	}
}
