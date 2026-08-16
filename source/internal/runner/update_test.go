package runner_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/tumika/tumika/source/internal/domain"
	"github.com/tumika/tumika/source/internal/platform/release"
	"github.com/tumika/tumika/source/internal/runner"
	"github.com/tumika/tumika/source/internal/service"
)

// fakeUpdates is an UpdateService the test drives.
type fakeUpdates struct {
	mu        sync.Mutex
	available string
	newer     bool
	checkErr  error
	applyErr  error
	applied   []string
	checks    int
}

func (f *fakeUpdates) State(context.Context) (domain.UpdateState, error) {
	return domain.UpdateState{Status: domain.UpdateIdle}, nil
}

func (f *fakeUpdates) Check(context.Context) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checks++
	return f.available, f.newer, f.checkErr
}

func (f *fakeUpdates) Apply(_ context.Context, version string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.applyErr != nil {
		return f.applyErr
	}
	f.applied = append(f.applied, version)
	return nil
}

func (f *fakeUpdates) ConfirmBoot(context.Context) (bool, error) { return false, nil }
func (f *fakeUpdates) Confirm(context.Context) error             { return nil }

func (f *fakeUpdates) appliedVersions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.applied...)
}

func (f *fakeUpdates) checkCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.checks
}

// fakeConfig answers only the auto-apply key — which is all the runner's
// narrowed dependency asks for.
type fakeConfig struct {
	auto bool
	err  error
}

func (f *fakeConfig) Get(_ context.Context, key string) (domain.SettingView, error) {
	if f.err != nil {
		return domain.SettingView{}, f.err
	}
	value, _ := json.Marshal(f.auto)
	return domain.SettingView{Key: key, Value: value}, nil
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// waitFor polls until cond holds, so the tests do not depend on a fixed sleep.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// A newer release with auto-apply ON is installed, and the runner signals that
// the process should restart onto it.
func TestAutoApplyInstallsAndSignals(t *testing.T) {
	updates := &fakeUpdates{available: "0.2.0", newer: true}
	r := runner.NewUpdate(updates, &fakeConfig{auto: true}, quietLogger(), time.Hour, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = r.Start(ctx) }()

	select {
	case <-r.Exit():
	case <-time.After(5 * time.Second):
		t.Fatal("the runner never signalled a restart")
	}

	if got := updates.appliedVersions(); len(got) != 1 || got[0] != "0.2.0" {
		t.Errorf("applied %v, want [0.2.0]", got)
	}
}

// With auto-apply OFF the operator is TOLD, not acted upon. Applying anyway
// would restart a daemon on someone else's schedule.
func TestAutoApplyOffInstallsNothing(t *testing.T) {
	updates := &fakeUpdates{available: "0.2.0", newer: true}
	r := runner.NewUpdate(updates, &fakeConfig{auto: false}, quietLogger(), time.Hour, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = r.Start(ctx) }()

	waitFor(t, "the first check", func() bool { return updates.checkCount() > 0 })
	time.Sleep(50 * time.Millisecond)

	if got := updates.appliedVersions(); len(got) != 0 {
		t.Errorf("applied %v with auto-apply off", got)
	}
	select {
	case <-r.Exit():
		t.Error("the runner signalled a restart without applying anything")
	default:
	}
}

// Nothing newer means nothing happens.
func TestNothingHappensWhenUpToDate(t *testing.T) {
	updates := &fakeUpdates{available: "0.1.0", newer: false}
	r := runner.NewUpdate(updates, &fakeConfig{auto: true}, quietLogger(), time.Hour, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = r.Start(ctx) }()

	waitFor(t, "the first check", func() bool { return updates.checkCount() > 0 })
	time.Sleep(50 * time.Millisecond)

	if got := updates.appliedVersions(); len(got) != 0 {
		t.Errorf("applied %v when already up to date", got)
	}
}

// An update check is a background convenience. GitHub being unreachable must
// never take down a daemon whose actual job is running workflows.
func TestAFailedCheckDoesNotStopTheRunner(t *testing.T) {
	updates := &fakeUpdates{checkErr: errors.New("connection reset")}
	r := runner.NewUpdate(updates, &fakeConfig{auto: true}, quietLogger(), 20*time.Millisecond, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- r.Start(ctx) }()

	// It has to keep ticking, not give up after the first failure.
	waitFor(t, "several checks", func() bool { return updates.checkCount() >= 3 })

	select {
	case err := <-done:
		t.Fatalf("the runner exited on a failed check: %v", err)
	default:
	}
}

// A repository with no releases is normal before the first tag, and must not be
// logged as a warning every interval forever.
func TestNoReleaseIsNotTreatedAsAFailure(t *testing.T) {
	updates := &fakeUpdates{checkErr: release.ErrNoRelease}
	r := runner.NewUpdate(updates, &fakeConfig{auto: true}, quietLogger(), 20*time.Millisecond, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = r.Start(ctx) }()

	waitFor(t, "several checks", func() bool { return updates.checkCount() >= 2 })
	if got := updates.appliedVersions(); len(got) != 0 {
		t.Errorf("applied %v with no release published", got)
	}
}

// A failed apply leaves the daemon on the version it is running, and does NOT
// signal a restart — restarting onto a binary that was never installed is how a
// daemon stops coming back.
func TestAFailedApplyDoesNotSignalARestart(t *testing.T) {
	updates := &fakeUpdates{
		available: "0.2.0", newer: true,
		applyErr: errors.New("checksum mismatch"),
	}
	r := runner.NewUpdate(updates, &fakeConfig{auto: true}, quietLogger(), 20*time.Millisecond, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = r.Start(ctx) }()

	waitFor(t, "an apply attempt", func() bool { return updates.checkCount() >= 2 })

	select {
	case <-r.Exit():
		t.Error("a failed apply signalled a restart")
	default:
	}
}

// Cancelling the context stops it promptly — a shutdown must not wait out the
// check interval, which is hours.
func TestCancellationStopsTheRunner(t *testing.T) {
	updates := &fakeUpdates{available: "0.1.0"}
	r := runner.NewUpdate(updates, &fakeConfig{auto: false}, quietLogger(), time.Hour, 0)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Start(ctx) }()

	waitFor(t, "the first check", func() bool { return updates.checkCount() > 0 })
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Start returned %v on cancellation, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the runner did not stop when cancelled")
	}
}

// The jitter must not swallow cancellation: a daemon told to stop during its
// startup delay has to stop.
func TestCancellationDuringJitter(t *testing.T) {
	updates := &fakeUpdates{}
	r := runner.NewUpdate(updates, &fakeConfig{auto: false}, quietLogger(), time.Hour, time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Start(ctx) }()

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the runner waited out its jitter instead of stopping")
	}
	if updates.checkCount() != 0 {
		t.Error("a check ran despite cancellation during the jitter")
	}
}

func TestName(t *testing.T) {
	r := runner.NewUpdate(&fakeUpdates{}, &fakeConfig{}, quietLogger(), time.Hour, 0)
	if r.Name() != "update" {
		t.Errorf("Name = %q", r.Name())
	}
	if err := r.Stop(context.Background()); err != nil {
		t.Errorf("Stop: %v", err)
	}
}

var _ service.UpdateService = (*fakeUpdates)(nil)
var _ service.SettingGetter = (*fakeConfig)(nil)
