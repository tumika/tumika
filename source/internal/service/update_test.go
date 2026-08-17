package service_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	if err := os.WriteFile(dest, []byte(body), 0o755); err != nil { //nolint:gosec // a stand-in for an executable
		return err
	}
	// Chmod explicitly, because WriteFile does NOT apply its mode to a file that
	// already exists — and the real Fetch guarantees an executable file at dest.
	// Without this the fake is more permissive than production in one direction
	// and less in another, which is how a fake stops testing anything.
	return os.Chmod(dest, 0o755) //nolint:gosec // a stand-in for an executable
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

// State passes the row through untouched — the API reports it verbatim.
func TestStateReportsTheRow(t *testing.T) {
	h := newHarness(t, "0.1.0")
	h.repo.state = domain.UpdateState{
		Status: domain.UpdatePending, FromVersion: "0.1.0", ToVersion: "0.2.0", BootAttempts: 2,
	}

	got, err := h.svc.State(t.Context())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if got.Status != domain.UpdatePending || got.BootAttempts != 2 {
		t.Errorf("State = %+v", got)
	}
}

// A failed Check must not be mistaken for "up to date", which would silently
// stop a fleet from ever updating.
func TestCheckReportsItsFailure(t *testing.T) {
	h := newHarness(t, "0.1.0")
	h.source.latestErr = errors.New("connection reset")

	if _, newer, err := h.svc.Check(t.Context()); err == nil {
		t.Fatal("a failed check reported success")
	} else if newer {
		t.Error("a failed check reported an update as available")
	}
}

// BinaryPath resolves symlinks, because the update replaces a FILE: renaming
// over a symlink would replace the link and leave the real binary untouched, so
// the daemon would relaunch on the old one and loop.
func TestBinaryPathResolvesSymlinks(t *testing.T) {
	got, err := service.BinaryPath()
	if err != nil {
		t.Fatalf("BinaryPath: %v", err)
	}
	if got == "" || !filepath.IsAbs(got) {
		t.Errorf("BinaryPath = %q, want an absolute path", got)
	}
	resolved, err := filepath.EvalSymlinks(got)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	if resolved != got {
		t.Errorf("BinaryPath returned %q, which still resolves to %q", got, resolved)
	}
}

// The pre-flight parses `tumika version` output. It is the entire evidence that
// the downloaded binary works, so anything unexpected has to be refused rather
// than read optimistically.
func TestPreflightRefusesUnexpectedVersionOutput(t *testing.T) {
	for _, output := range []string{
		"",
		"tumika",
		"not tumika at all",
		"Segmentation fault",
	} {
		h := newHarness(t, "0.1.0", service.WithUpdateExec(
			func(context.Context, string) (string, error) { return output, nil }))

		if err := h.svc.Apply(t.Context(), "0.2.0"); err == nil {
			t.Errorf("output %q was accepted as a working binary", output)
		}
		if got := h.read(t, h.binary); got != "binary 0.1.0" {
			t.Errorf("output %q: the running binary was replaced", output)
		}
	}
}

// A failure to set the old binary aside stops the update, with the running
// binary untouched.
//
// The first rename is the one that can realistically fail — an unwritable
// directory, or the file gone. Making the SECOND rename fail is not portably
// arrangeable: putting a directory at the destination does not do it, because
// the first rename moves that directory aside and frees the path. Worth
// recording rather than faking, since the restore-on-failure branch below it is
// therefore not covered by a test.
func TestApplyStopsIfTheOldBinaryCannotBeSetAside(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "tumika")
	if err := os.WriteFile(binary, []byte("binary 0.1.0"), 0o755); err != nil { //nolint:gosec // a stand-in for an executable
		t.Fatalf("write: %v", err)
	}

	repo := newUpdateRepo()
	// The pre-flight runs between the fetch and the renames, so it is where the
	// binary can be made to disappear.
	svc := service.NewUpdateService(repo, &fakeSource{contents: map[string]string{}},
		&fakeTxer{}, "0.1.0", binary,
		service.WithUpdateExec(func(context.Context, string) (string, error) {
			_ = os.Remove(binary)
			return "tumika 0.2.0 (commit abc, built now, go1.26.6, linux/arm64)\n", nil
		}))

	if err := svc.Apply(t.Context(), "0.2.0"); err == nil {
		t.Fatal("Apply succeeded despite being unable to set the old binary aside")
	}
	// The pending row was already written — which is the correct order, and
	// ConfirmBoot resolves it harmlessly on the next boot.
	if repo.state.Status != domain.UpdatePending {
		t.Errorf("status = %q, want pending recorded before the swap", repo.state.Status)
	}
}

// Confirm tolerates a missing .old: the update is confirmed either way, and a
// leftover or absent fallback costs one binary's disk. Failing here would turn a
// successful update into a reported failure.
func TestConfirmToleratesAMissingFallback(t *testing.T) {
	h := newHarness(t, "0.2.0")
	h.repo.state = domain.UpdateState{Status: domain.UpdatePending, ToVersion: "0.2.0"}

	if err := h.svc.Confirm(t.Context()); err != nil {
		t.Fatalf("Confirm failed with no .old present: %v", err)
	}
	if h.repo.state.Status != domain.UpdateConfirmed {
		t.Errorf("status = %q, want confirmed", h.repo.state.Status)
	}
}

// A failure to record the confirmation is reported — the row is the only thing
// that tells the next boot the update succeeded.
func TestConfirmReportsAFailedWrite(t *testing.T) {
	h := newHarness(t, "0.2.0")
	h.repo.state = domain.UpdateState{Status: domain.UpdatePending, ToVersion: "0.2.0"}
	h.repo.putErr = errors.New("database is locked")

	if err := h.svc.Confirm(t.Context()); err == nil {
		t.Fatal("a failed write reported success")
	}
}

// A failure to read the row at boot is reported rather than silently treated as
// "nothing pending", which would skip a rollback that was due.
func TestConfirmBootReportsAFailedIncrement(t *testing.T) {
	h := newHarness(t, "0.2.0")
	h.repo.state = domain.UpdateState{Status: domain.UpdatePending, ToVersion: "0.2.0"}
	h.repo.incrErr = errors.New("database is locked")

	if _, err := h.svc.ConfirmBoot(t.Context()); err == nil {
		t.Fatal("a failed increment reported success")
	}
}

// execVersion is the real pre-flight, exercised without the injected stub so
// the exec plumbing itself is covered rather than only the parsing.
func TestApplyWithTheRealPreflightExec(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "tumika")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\necho old\n"), 0o755); err != nil { //nolint:gosec // a stand-in for an executable
		t.Fatalf("write: %v", err)
	}

	// What the "downloaded" binary will be: a script that reports 0.2.0, which
	// is exactly what the pre-flight has to read back.
	script := "#!/bin/sh\necho 'tumika 0.2.0 (commit abc, built now, go1.26.6, linux/arm64)'\n"

	svc := service.NewUpdateService(newUpdateRepo(),
		&fakeSource{contents: map[string]string{"0.2.0": script}},
		&fakeTxer{}, "0.1.0", binary)

	if err := svc.Apply(t.Context(), "0.2.0"); err != nil {
		t.Fatalf("Apply with the real exec: %v", err)
	}
	body, err := os.ReadFile(binary) //nolint:gosec // a path this test created
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(body) != script {
		t.Error("the staged binary was not installed")
	}
}

// And a staged binary that is not executable fails the pre-flight rather than
// being installed — the checksum cannot tell the difference.
func TestApplyWithARealExecThatCannotRun(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "tumika")
	if err := os.WriteFile(binary, []byte("old"), 0o755); err != nil { //nolint:gosec // a stand-in for an executable
		t.Fatalf("write: %v", err)
	}

	svc := service.NewUpdateService(newUpdateRepo(),
		&fakeSource{contents: map[string]string{"0.2.0": "not a program at all"}},
		&fakeTxer{}, "0.1.0", binary)

	if err := svc.Apply(t.Context(), "0.2.0"); err == nil {
		t.Fatal("a file that is not a program was installed")
	}
	body, _ := os.ReadFile(binary) //nolint:gosec // a path this test created
	if string(body) != "old" {
		t.Error("the running binary was replaced by something that cannot execute")
	}
}

// THE bug that would have bricked a daemon.
//
// After a successful apply this process still reports the OLD version — it is
// still executing the old binary — so release.Newer stays true and a second
// Apply used to sail straight through. It moved the NEW binary to .old and
// installed the new one again, so .old held the broken version and a rollback
// "restored" it. The daemon would crash-loop with no way back.
//
// Reachable from `tumika update` run twice, two concurrent POSTs, or the runner
// racing the API.
func TestASecondApplyCannotDestroyTheRollbackFallback(t *testing.T) {
	h := newHarness(t, "0.1.0")

	if err := h.svc.Apply(t.Context(), "0.2.0"); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	if got := h.read(t, h.binary+".old"); got != "binary 0.1.0" {
		t.Fatalf(".old is %q after the first apply", got)
	}

	err := h.svc.Apply(t.Context(), "0.2.0")
	if err == nil {
		t.Fatal("a second apply was accepted while an update was already pending")
	}
	if !errors.Is(err, domain.ErrConflict) {
		t.Errorf("= %v, want ErrConflict", err)
	}
	// Two guards can stop this — the pending check and the refusal to clobber a
	// stale .old — and they are NOT interchangeable to the person reading the
	// message. Someone who ran `tumika update` twice needs "restart the
	// service", not "remove this file once you are satisfied". Asserting the
	// message is what keeps the pending guard load-bearing; without it, deleting
	// that guard leaves every test green.
	if !strings.Contains(err.Error(), "waiting for a restart") {
		t.Errorf("the error tells the operator the wrong thing to do: %v", err)
	}

	// The fallback still points at the version that is known to work.
	if got := h.read(t, h.binary+".old"); got != "binary 0.1.0" {
		t.Errorf(".old is %q; the rollback fallback was destroyed", got)
	}
}

// Concurrent applies are serialised and only one wins. Without the lock two
// downloads race on the same staging path and both reach the renames.
func TestConcurrentAppliesAreSerialised(t *testing.T) {
	h := newHarness(t, "0.1.0")

	const callers = 5
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- h.svc.Apply(context.Background(), "0.2.0")
		}()
	}
	wg.Wait()
	close(errs)

	succeeded := 0
	for err := range errs {
		if err == nil {
			succeeded++
		}
	}
	if succeeded != 1 {
		t.Errorf("%d of %d concurrent applies succeeded, want exactly 1", succeeded, callers)
	}
	if got := h.read(t, h.binary+".old"); got != "binary 0.1.0" {
		t.Errorf(".old is %q; a concurrent apply destroyed the fallback", got)
	}
}

// A stale .old with no pending update means the row and the filesystem
// disagree. Overwriting it is how the fallback is lost, so it is refused.
func TestApplyRefusesToClobberAStaleFallback(t *testing.T) {
	h := newHarness(t, "0.1.0")
	if err := os.WriteFile(h.binary+".old", []byte("binary 0.0.9"), 0o755); err != nil { //nolint:gosec // a stand-in for an executable
		t.Fatalf("write: %v", err)
	}

	if err := h.svc.Apply(t.Context(), "0.2.0"); err == nil {
		t.Fatal("a stale fallback was overwritten")
	}
	if got := h.read(t, h.binary+".old"); got != "binary 0.0.9" {
		t.Errorf(".old is %q, want the stale one left untouched", got)
	}
	if got := h.read(t, h.binary); got != "binary 0.1.0" {
		t.Error("the running binary was replaced")
	}
}

// The rollback row is written BEFORE the irreversible rename.
//
// Renaming first and failing to persist left the row saying `pending`, which
// Confirm — during the very same startup — promoted to `confirmed`. An operator
// would see "confirmed 0.1.0 → 0.2.0" on a machine running 0.1.0, with the
// fallback deleted.
func TestRollbackRecordsBeforeItRenames(t *testing.T) {
	h := newHarness(t, "0.2.0")
	if err := os.WriteFile(h.binary+".old", []byte("binary 0.1.0"), 0o755); err != nil { //nolint:gosec // a stand-in for an executable
		t.Fatalf("write: %v", err)
	}
	h.repo.state = domain.UpdateState{
		Status: domain.UpdatePending, FromVersion: "0.1.0", ToVersion: "0.2.0",
		BootAttempts: domain.MaxBootAttempts - 1,
	}
	h.repo.putErr = errors.New("database is locked")

	if _, err := h.svc.ConfirmBoot(t.Context()); err == nil {
		t.Fatal("a failed write reported a successful rollback")
	}
	// The rename did not happen, so the state and the filesystem still agree:
	// the new binary is in place and still pending.
	if got := h.read(t, h.binary); got != "binary 0.2.0" {
		t.Errorf("the binary was swapped despite the state write failing: %q", got)
	}
}

// A confirmed update whose fallback cannot be removed is still CONFIRMED — but
// it says so, because the next Apply refuses to overwrite a stale .old.
func TestConfirmReportsAFallbackItCannotRemove(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the permission bits this relies on")
	}

	dir := t.TempDir()
	binary := filepath.Join(dir, "tumika")
	for _, path := range []string{binary, binary + ".old"} {
		if err := os.WriteFile(path, []byte("binary"), 0o755); err != nil { //nolint:gosec // a stand-in for an executable
			t.Fatalf("write: %v", err)
		}
	}
	// A read-only directory blocks the unlink without blocking the state write.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	repo := newUpdateRepo()
	repo.state = domain.UpdateState{Status: domain.UpdatePending, ToVersion: "0.2.0"}
	svc := service.NewUpdateService(repo, &fakeSource{contents: map[string]string{}},
		&fakeTxer{}, "0.2.0", binary)

	err := svc.Confirm(t.Context())
	if !errors.Is(err, service.ErrFallbackNotRemoved) {
		t.Fatalf("= %v, want ErrFallbackNotRemoved", err)
	}
	// The update is confirmed regardless: the row is what matters.
	if repo.state.Status != domain.UpdateConfirmed {
		t.Errorf("status = %q, want confirmed", repo.state.Status)
	}
}
