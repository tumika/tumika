package service_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tumika/tumika/source/internal/domain"
	"github.com/tumika/tumika/source/internal/service"
)

// fakeSource is a release feed the test controls.
type fakeSource struct {
	latest    string
	latestErr error
	// contents is what Fetch writes, keyed by version.
	contents map[string]string
	fetchErr error
	fetched  []string
}

func (f *fakeSource) Latest(context.Context) (string, error) {
	return f.latest, f.latestErr
}

func (f *fakeSource) Fetch(_ context.Context, version, dest string) error {
	f.fetched = append(f.fetched, version)
	if f.fetchErr != nil {
		return f.fetchErr
	}
	body, ok := f.contents[version]
	if !ok {
		body = "binary " + version
	}
	return os.WriteFile(dest, []byte(body), 0o755) //nolint:gosec // a stand-in for an executable
}

// fakeUpdateRepo is the single-row state table, in memory.
type fakeUpdateRepo struct {
	state   domain.UpdateState
	putErr  error
	incrErr error
}

func newUpdateRepo() *fakeUpdateRepo {
	return &fakeUpdateRepo{state: domain.UpdateState{Status: domain.UpdateIdle}}
}

func (f *fakeUpdateRepo) Get(context.Context) (domain.UpdateState, error) {
	return f.state, nil
}

func (f *fakeUpdateRepo) Put(_ context.Context, s domain.UpdateState) error {
	if f.putErr != nil {
		return f.putErr
	}
	f.state = s
	return nil
}

func (f *fakeUpdateRepo) IncrementBootAttempts(context.Context) (domain.UpdateState, error) {
	if f.incrErr != nil {
		return domain.UpdateState{}, f.incrErr
	}
	f.state.BootAttempts++
	return f.state, nil
}

// harness builds a service over a real binary path in a temp directory, so the
// rename dance is exercised against a filesystem rather than mocked away.
type harness struct {
	svc    service.UpdateService
	repo   *fakeUpdateRepo
	source *fakeSource
	binary string
	// preflight is what the injected exec reports. Empty means "match whatever
	// was asked for", which is the healthy case.
	preflight    string
	preflightErr error
}

func newHarness(t *testing.T, current string, opts ...service.UpdateOption) *harness {
	t.Helper()

	dir := t.TempDir()
	binary := filepath.Join(dir, "tumika")
	if err := os.WriteFile(binary, []byte("binary "+current), 0o755); err != nil { //nolint:gosec // a stand-in for an executable
		t.Fatalf("write: %v", err)
	}

	h := &harness{
		repo:   newUpdateRepo(),
		source: &fakeSource{contents: map[string]string{}},
		binary: binary,
	}

	base := []service.UpdateOption{
		service.WithUpdateExec(func(_ context.Context, path string) (string, error) {
			if h.preflightErr != nil {
				return "", h.preflightErr
			}
			body, err := os.ReadFile(path) //nolint:gosec // a path this test created
			if err != nil {
				return "", err
			}
			reported := h.preflight
			if reported == "" {
				// The staged file is "binary <version>"; report that, which is
				// what a healthy build would say about itself.
				reported = strings.TrimPrefix(string(body), "binary ")
			}
			return "tumika " + reported + " (commit abc, built now, go1.26.6, linux/arm64)\n", nil
		}),
	}
	h.svc = service.NewUpdateService(h.repo, h.source, &fakeTxer{}, current, binary, append(base, opts...)...)
	return h
}

func (h *harness) read(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path) //nolint:gosec // a path this test created
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(body)
}

// THE ordering property. Every step has to happen before the replacement, or
// the daemon ends up on a binary it cannot run with no record of why.
func TestApplyReplacesTheBinaryAndKeepsTheOldOne(t *testing.T) {
	h := newHarness(t, "0.1.0")
	h.source.latest = "0.2.0"

	if err := h.svc.Apply(t.Context(), "0.2.0"); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if got := h.read(t, h.binary); got != "binary 0.2.0" {
		t.Errorf("the binary is %q, want the new one", got)
	}
	// The old binary is the ONLY thing a rollback can restore from, and the
	// boot after a failed update is exactly when the network cannot be assumed.
	if got := h.read(t, h.binary+".old"); got != "binary 0.1.0" {
		t.Errorf("the kept binary is %q, want the previous one", got)
	}

	state, _ := h.repo.Get(t.Context())
	if state.Status != domain.UpdatePending {
		t.Errorf("status = %q, want pending", state.Status)
	}
	if state.FromVersion != "0.1.0" || state.ToVersion != "0.2.0" {
		t.Errorf("state = %+v, want 0.1.0 -> 0.2.0", state)
	}
	if state.BootAttempts != 0 {
		t.Errorf("boot attempts = %d, want 0", state.BootAttempts)
	}
}

// The pre-flight runs while the OLD binary is still in charge. A checksum
// proves the bytes are the published ones; it does not prove they execute — a
// build for the wrong architecture hashes perfectly and cannot run.
func TestApplyRefusesABinaryThatWillNotRun(t *testing.T) {
	h := newHarness(t, "0.1.0")
	h.preflightErr = errors.New("exec format error")

	err := h.svc.Apply(t.Context(), "0.2.0")
	if err == nil {
		t.Fatal("a binary that cannot execute was installed")
	}

	if got := h.read(t, h.binary); got != "binary 0.1.0" {
		t.Errorf("the running binary was replaced anyway: %q", got)
	}
	if _, statErr := os.Stat(h.binary + ".old"); statErr == nil {
		t.Error("a rollback copy was made for an update that never happened")
	}
	state, _ := h.repo.Get(t.Context())
	if state.Status != domain.UpdateIdle {
		t.Errorf("status = %q, want idle — nothing was installed", state.Status)
	}
}

// A binary that runs but reports a different version is not the release that
// was asked for. Installing it would leave the daemon claiming a version it is
// not, and the next update comparison would be wrong.
func TestApplyRefusesAVersionMismatch(t *testing.T) {
	h := newHarness(t, "0.1.0")
	h.preflight = "0.9.9"

	err := h.svc.Apply(t.Context(), "0.2.0")
	if err == nil {
		t.Fatal("a binary reporting the wrong version was installed")
	}
	if !strings.Contains(err.Error(), "0.9.9") {
		t.Errorf("the error does not name what it found: %v", err)
	}
	if got := h.read(t, h.binary); got != "binary 0.1.0" {
		t.Error("the running binary was replaced")
	}
}

// Strictly greater. A daemon must never "update" onto the version it is already
// running, and must never downgrade because someone deleted a release.
func TestApplyRefusesAnythingNotNewer(t *testing.T) {
	h := newHarness(t, "0.2.0")

	for _, version := range []string{"0.2.0", "0.1.0", "0.1.9"} {
		if err := h.svc.Apply(t.Context(), version); !errors.Is(err, domain.ErrConflict) {
			t.Errorf("Apply(%s) = %v, want ErrConflict", version, err)
		}
		if len(h.source.fetched) != 0 {
			t.Errorf("Apply(%s) downloaded something anyway: %v", version, h.source.fetched)
		}
	}
}

// The state row is written BEFORE the replacement. A crash between the two
// leaves `pending` against a binary that was never swapped, which ConfirmBoot
// resolves harmlessly — the reverse order leaves a swapped binary with no
// record, and nothing would ever roll it back.
func TestApplyRecordsPendingBeforeReplacing(t *testing.T) {
	h := newHarness(t, "0.1.0")
	h.repo.putErr = errors.New("database is locked")

	if err := h.svc.Apply(t.Context(), "0.2.0"); err == nil {
		t.Fatal("Apply succeeded despite failing to record the pending state")
	}
	if got := h.read(t, h.binary); got != "binary 0.1.0" {
		t.Errorf("the binary was replaced without a record: %q", got)
	}
}

// A boot that is not pending changes nothing. This runs on EVERY start, so it
// has to be inert in the overwhelmingly common case.
func TestConfirmBootIsInertWhenNothingIsPending(t *testing.T) {
	h := newHarness(t, "0.2.0")

	rolledBack, err := h.svc.ConfirmBoot(t.Context())
	if err != nil {
		t.Fatalf("ConfirmBoot: %v", err)
	}
	if rolledBack {
		t.Error("an idle state was treated as a rollback")
	}
	if h.repo.state.BootAttempts != 0 {
		t.Errorf("boot attempts = %d on an idle state", h.repo.state.BootAttempts)
	}
}

// The counter increments on every boot of a pending update, and the rollback
// fires only at the limit. Counting AFTER a successful start would never catch
// a binary that dies during startup — which is the failure being guarded.
func TestConfirmBootRollsBackAfterEnoughFailures(t *testing.T) {
	h := newHarness(t, "0.2.0")
	if err := os.WriteFile(h.binary+".old", []byte("binary 0.1.0"), 0o755); err != nil { //nolint:gosec // a stand-in for an executable
		t.Fatalf("write: %v", err)
	}
	h.repo.state = domain.UpdateState{
		Status: domain.UpdatePending, FromVersion: "0.1.0", ToVersion: "0.2.0",
	}

	for attempt := 1; attempt < domain.MaxBootAttempts; attempt++ {
		rolledBack, err := h.svc.ConfirmBoot(t.Context())
		if err != nil {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
		if rolledBack {
			t.Fatalf("rolled back after %d attempts, before the limit of %d",
				attempt, domain.MaxBootAttempts)
		}
	}

	rolledBack, err := h.svc.ConfirmBoot(t.Context())
	if err != nil {
		t.Fatalf("ConfirmBoot: %v", err)
	}
	if !rolledBack {
		t.Fatalf("no rollback after %d attempts", domain.MaxBootAttempts)
	}
	if got := h.read(t, h.binary); got != "binary 0.1.0" {
		t.Errorf("the binary is %q, want the restored previous one", got)
	}
	if h.repo.state.Status != domain.UpdateRolledBack {
		t.Errorf("status = %q, want rolled_back", h.repo.state.Status)
	}
}

// A pending update with no .old is unrecoverable. It must still be recorded as
// rolled_back so the daemon stops trying — looping forever on a pending update
// with no fallback is the worst of both outcomes.
func TestConfirmBootWithNothingToRollBackTo(t *testing.T) {
	h := newHarness(t, "0.2.0")
	h.repo.state = domain.UpdateState{
		Status: domain.UpdatePending, ToVersion: "0.2.0",
		BootAttempts: domain.MaxBootAttempts - 1,
	}

	_, err := h.svc.ConfirmBoot(t.Context())
	if err == nil {
		t.Fatal("a missing rollback binary was not reported")
	}
	if h.repo.state.Status != domain.UpdateRolledBack {
		t.Errorf("status = %q, want rolled_back so the daemon stops trying", h.repo.state.Status)
	}
}

// Confirm runs once the daemon is SERVING, and only then removes the fallback.
// Deleting .old at startup would throw away the rollback while the evidence
// that the new binary works was still missing.
func TestConfirmMarksSuccessAndRemovesTheFallback(t *testing.T) {
	h := newHarness(t, "0.2.0")
	if err := os.WriteFile(h.binary+".old", []byte("binary 0.1.0"), 0o755); err != nil { //nolint:gosec // a stand-in for an executable
		t.Fatalf("write: %v", err)
	}
	h.repo.state = domain.UpdateState{Status: domain.UpdatePending, ToVersion: "0.2.0"}

	if err := h.svc.Confirm(t.Context()); err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if h.repo.state.Status != domain.UpdateConfirmed {
		t.Errorf("status = %q, want confirmed", h.repo.state.Status)
	}
	if _, err := os.Stat(h.binary + ".old"); !errors.Is(err, os.ErrNotExist) {
		t.Error("the fallback binary survived a confirmed update")
	}
}

// Confirming when nothing is pending must not touch anything — it runs on every
// serve.
func TestConfirmIsInertWhenNothingIsPending(t *testing.T) {
	h := newHarness(t, "0.2.0")
	if err := os.WriteFile(h.binary+".old", []byte("binary 0.1.0"), 0o755); err != nil { //nolint:gosec // a stand-in for an executable
		t.Fatalf("write: %v", err)
	}

	if err := h.svc.Confirm(t.Context()); err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if _, err := os.Stat(h.binary + ".old"); err != nil {
		t.Error("Confirm removed a fallback for an update that was not pending")
	}
}

func TestCheckReportsWhatIsNewer(t *testing.T) {
	h := newHarness(t, "0.1.0")

	h.source.latest = "0.2.0"
	available, newer, err := h.svc.Check(t.Context())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if available != "0.2.0" || !newer {
		t.Errorf("Check = %q/%v, want 0.2.0/true", available, newer)
	}

	h.source.latest = "0.1.0"
	if _, newer, _ = h.svc.Check(t.Context()); newer {
		t.Error("the running version was reported as out of date")
	}
}

// A failed download leaves nothing behind — no staged file, no state change,
// and the running binary untouched.
func TestApplyLeavesNothingBehindOnAFailedDownload(t *testing.T) {
	h := newHarness(t, "0.1.0")
	h.source.fetchErr = errors.New("connection reset")

	if err := h.svc.Apply(t.Context(), "0.2.0"); err == nil {
		t.Fatal("a failed download reported success")
	}
	if got := h.read(t, h.binary); got != "binary 0.1.0" {
		t.Error("the running binary was touched")
	}
	if _, err := os.Stat(h.binary + ".new"); !errors.Is(err, os.ErrNotExist) {
		t.Error("a staged file was left behind")
	}
	if h.repo.state.Status != domain.UpdateIdle {
		t.Errorf("status = %q, want idle", h.repo.state.Status)
	}
}

// The full round trip: apply, then the boot that confirms it.
func TestTheHappyPathEndToEnd(t *testing.T) {
	started := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	h := newHarness(t, "0.1.0", service.WithUpdateClock(func() time.Time { return started }))

	if err := h.svc.Apply(t.Context(), "0.2.0"); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// The next process boots on the new binary.
	rolledBack, err := h.svc.ConfirmBoot(t.Context())
	if err != nil {
		t.Fatalf("ConfirmBoot: %v", err)
	}
	if rolledBack {
		t.Fatal("a healthy first boot rolled back")
	}
	if h.repo.state.BootAttempts != 1 {
		t.Errorf("boot attempts = %d, want 1", h.repo.state.BootAttempts)
	}

	// And then it serves.
	if err := h.svc.Confirm(t.Context()); err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if h.repo.state.Status != domain.UpdateConfirmed {
		t.Errorf("status = %q, want confirmed", h.repo.state.Status)
	}
	if _, err := os.Stat(h.binary + ".old"); !errors.Is(err, os.ErrNotExist) {
		t.Error("the fallback survived a confirmed update")
	}
}
