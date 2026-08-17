package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tumika/tumika/source/internal/domain"
	"github.com/tumika/tumika/source/internal/platform/release"
	"github.com/tumika/tumika/source/internal/repository"
)

// preflightTimeout bounds the `<new> version` exec. It is a local process
// printing one line; anything slower is stuck.
const preflightTimeout = 10 * time.Second

// oldSuffix names the binary kept for a rollback.
const oldSuffix = ".old"

// ErrFallbackNotRemoved means an update was confirmed but the previous binary
// could not be deleted.
//
// A distinct error because the UPDATE succeeded: the caller logs this and
// carries on rather than treating a completed update as a failure. It matters
// because the next Apply refuses to overwrite a stale .old.
var ErrFallbackNotRemoved = errors.New("the previous binary could not be removed")

// UpdateService owns the self-update state machine.
//
// The whole design turns on one constraint: a process cannot replace the binary
// it is executing and keep running (ADR-0003). So an update is two halves either
// side of a restart, and the seam between them is a database row — which is why
// it cannot live in memory.
//
//	apply:   pre-flight → mark pending → replace → exit 0 → supervisor relaunches
//	boot:    ConfirmBoot → confirmed, or after MaxBootAttempts → roll back
type UpdateService interface {
	// State is what the API reports.
	State(ctx context.Context) (domain.UpdateState, error)

	// Check reports the newest published version, and whether it is newer than
	// this binary. It touches nothing.
	Check(ctx context.Context) (available string, newer bool, err error)

	// Apply downloads, verifies, pre-flights and installs a version, then
	// returns. It does NOT exit the process — the caller decides when, because
	// only the caller knows whether a request is still in flight.
	Apply(ctx context.Context, version string) error

	// ConfirmBoot runs at startup, before serving.
	//
	// It resolves whatever the previous process left behind: a pending update
	// stays pending until it proves itself, or — after enough failed boots — the
	// previous binary is restored. Returns true when the caller should exit so
	// the supervisor relaunches on the restored binary.
	ConfirmBoot(ctx context.Context) (rolledBack bool, err error)

	// Confirm marks a pending update as successful. Called once the daemon is
	// actually serving, not merely running: a binary that starts and then fails
	// its first request has not proven anything.
	Confirm(ctx context.Context) error
}

type updateService struct {
	// mu serialises the whole state machine.
	//
	// Apply, ConfirmBoot and Confirm all read the row, act on the filesystem,
	// and write the row back. Two Applies interleaving is not a theoretical
	// race: the scheduled runner and POST /v1/update/apply can both fire, and
	// each is a multi-second download followed by two renames.
	mu      sync.Mutex
	repo    repository.UpdateStateRepository
	source  release.Source
	tx      repository.Txer
	version string
	// binary is the path this daemon's executable lives at, and the path an
	// update replaces.
	binary string
	// exec runs the pre-flight. Injected so the check itself is testable
	// without building a second binary.
	exec func(ctx context.Context, path string) (string, error)
	now  func() time.Time
}

// UpdateOption configures the service.
type UpdateOption func(*updateService)

// WithUpdateExec replaces the pre-flight exec.
func WithUpdateExec(f func(ctx context.Context, path string) (string, error)) UpdateOption {
	return func(s *updateService) { s.exec = f }
}

// WithUpdateClock replaces the clock.
func WithUpdateClock(f func() time.Time) UpdateOption {
	return func(s *updateService) { s.now = f }
}

// NewUpdateService builds the service.
func NewUpdateService(
	repo repository.UpdateStateRepository,
	source release.Source,
	tx repository.Txer,
	version, binary string,
	opts ...UpdateOption,
) UpdateService {
	s := &updateService{
		repo:    repo,
		source:  source,
		tx:      tx,
		version: version,
		binary:  binary,
		exec:    execVersion,
		now:     func() time.Time { return time.Now().UTC() },
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

func (s *updateService) State(ctx context.Context) (domain.UpdateState, error) {
	return s.repo.Get(ctx)
}

func (s *updateService) Check(ctx context.Context) (string, bool, error) {
	latest, err := s.source.Latest(ctx)
	if err != nil {
		return "", false, err
	}
	return latest, release.Newer(latest, s.version), nil
}

// Apply installs a version, leaving the process running on the old one.
//
// The ordering is the entire safety property:
//
//  1. download and verify — a bad artifact never reaches the binary path
//  2. PRE-FLIGHT the new binary while the old one is still in charge
//  3. record `pending` BEFORE replacing, so a crash between the two is still
//     recoverable
//  4. keep the old binary as .old, then rename the new one into place
//
// Reversing 2 and 4 is the tempting simplification, and it is how you end up
// with a daemon that cannot start and no record that anything happened.
func (s *updateService) Apply(ctx context.Context, version string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	version = strings.TrimPrefix(version, "v")

	if !release.Newer(version, s.version) {
		return fmt.Errorf("%w: %s is not newer than the running %s",
			domain.ErrConflict, version, s.version)
	}

	// REFUSE when an update is already installed and waiting for its restart.
	//
	// This process still reports the OLD version — it is still executing the old
	// binary — so release.Newer above stays true after a successful apply, and a
	// second Apply would sail straight through. It would then move the NEW
	// binary to .old and install the new one again, so .old would hold the
	// broken version and a rollback would "restore" it. The daemon would
	// crash-loop with no way back.
	//
	// Reachable from an operator running `tumika update` twice, from two
	// concurrent POSTs, and from the runner racing the API — all of which end at
	// the same destroyed fallback.
	current, err := s.repo.Get(ctx)
	if err != nil {
		return fmt.Errorf("read the update state: %w", err)
	}
	if current.Status == domain.UpdatePending {
		return fmt.Errorf("%w: %s is already installed and waiting for a restart; restart the service to run it",
			domain.ErrConflict, current.ToVersion)
	}

	// A UNIQUE staging NAME, so two applies cannot stomp each other's download
	// and the deferred cleanup only ever removes this call's file.
	//
	// The file is removed immediately: only the name is wanted. Leaving the
	// 0600 file CreateTemp makes would have Fetch write into it and the
	// pre-flight then fail to execute it — the mode of an existing file is not
	// changed by a write.
	stagedFile, err := os.CreateTemp(filepath.Dir(s.binary), ".tumika-staged-*")
	if err != nil {
		return fmt.Errorf("stage an update next to %s: %w", s.binary, err)
	}
	staged := stagedFile.Name()
	_ = stagedFile.Close()
	_ = os.Remove(staged)
	defer func() { _ = os.Remove(staged) }()

	if err := s.source.Fetch(ctx, version, staged); err != nil {
		return err
	}

	// PRE-FLIGHT, while the old binary is still in charge.
	//
	// The checksum proves the bytes are the ones published. It does not prove
	// they run: a build for the wrong architecture, or one linked against a libc
	// this host does not have, hashes perfectly and cannot execute. Finding that
	// out AFTER the replacement means a daemon that exits 203 forever.
	reported, err := s.preflight(ctx, staged)
	if err != nil {
		return err
	}
	if reported != version {
		return fmt.Errorf("%w: the downloaded binary reports version %q, not %q",
			domain.ErrConflict, reported, version)
	}

	// Recorded BEFORE the replacement. A crash between this write and the
	// rename leaves `pending` against a binary that was never swapped — which
	// ConfirmBoot resolves harmlessly. The reverse order leaves a swapped binary
	// with no record, and nothing would ever roll it back.
	started := s.now()
	state := domain.UpdateState{
		Status:       domain.UpdatePending,
		FromVersion:  s.version,
		ToVersion:    version,
		BootAttempts: 0,
		StartedAt:    &started,
		UpdatedAt:    started,
	}
	if err := s.tx.InTx(ctx, func(ctx context.Context) error {
		return s.repo.Put(ctx, state)
	}); err != nil {
		return fmt.Errorf("record the pending update: %w", err)
	}

	// The old binary is KEPT, not overwritten. It is the only thing a rollback
	// has to restore from, and downloading it again would need the network to be
	// working — which, on the boot after a failed update, is exactly what cannot
	// be assumed.
	//
	// A pre-existing .old is left alone rather than clobbered: it belongs to the
	// version currently running, and replacing it is exactly how the fallback
	// gets destroyed. The pending guard above should make this unreachable, so
	// reaching it means the state row and the filesystem disagree — which is
	// worth refusing rather than papering over.
	if _, statErr := os.Stat(s.binary + oldSuffix); statErr == nil {
		return fmt.Errorf("%w: %s already exists but no update is pending; "+
			"remove it once you are satisfied the running version is good",
			domain.ErrConflict, s.binary+oldSuffix)
	}
	if err := os.Rename(s.binary, s.binary+oldSuffix); err != nil {
		return fmt.Errorf("keep the current binary as %s: %w", s.binary+oldSuffix, err)
	}
	if err := os.Rename(staged, s.binary); err != nil {
		// Put it back. Failing here with neither binary in place would leave
		// nothing for the supervisor to start.
		if restoreErr := os.Rename(s.binary+oldSuffix, s.binary); restoreErr != nil {
			return fmt.Errorf("install %s: %w (and restoring the previous binary failed: %v)",
				s.binary, err, restoreErr)
		}
		return fmt.Errorf("install %s: %w", s.binary, err)
	}
	return nil
}

// preflight execs the staged binary and reads the version it reports.
func (s *updateService) preflight(ctx context.Context, path string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, preflightTimeout)
	defer cancel()

	out, err := s.exec(ctx, path)
	if err != nil {
		return "", fmt.Errorf("%w: the downloaded binary could not be run: %w",
			domain.ErrConflict, err)
	}
	return parseVersion(out)
}

// execVersion runs `<path> version` and returns its output.
func execVersion(ctx context.Context, path string) (string, error) {
	// Both scanners flag the variable command. The audit: path is always the
	// staged file Apply just created next to tumika's own binary — it is not
	// caller-supplied, and the single argument is a literal. Running it is the
	// entire point of a pre-flight.
	//
	// nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
	cmd := exec.CommandContext(ctx, path, "version") // #nosec G204 -- see the audit above: path is the file Apply staged beside our own binary
	// A minimal environment: `version` must not depend on the daemon's own, and
	// the updater execs it precisely to learn whether it works in isolation.
	cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin"}

	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// parseVersion reads the version out of `tumika version` output.
//
// The format is "tumika <version> (commit …". Parsed rather than trusted
// wholesale because this string is the pre-flight's entire evidence.
func parseVersion(out string) (string, error) {
	first, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	fields := strings.Fields(first)
	if len(fields) < 2 || fields[0] != "tumika" {
		return "", fmt.Errorf("%w: unexpected `tumika version` output %q", domain.ErrConflict, first)
	}
	return fields[1], nil
}

// ConfirmBoot resolves whatever the previous process left behind.
//
// Called at startup, BEFORE serving. The counter is incremented first and read
// afterwards: a binary that crashes during startup must still have its attempt
// counted, or it would loop forever without ever reaching the rollback.
func (s *updateService) ConfirmBoot(ctx context.Context) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	state, err := s.repo.Get(ctx)
	if err != nil {
		return false, err
	}
	if state.Status != domain.UpdatePending {
		return false, nil
	}

	state, err = s.repo.IncrementBootAttempts(ctx)
	if err != nil {
		return false, err
	}

	if !state.ShouldRollBack() {
		// Booting, but not yet proven. Confirm() marks it once the daemon is
		// actually serving.
		return false, nil
	}

	// Enough failures. Restore the previous binary and exit so the supervisor
	// relaunches on it.
	if err := s.rollBack(ctx, state); err != nil {
		return false, err
	}
	return true, nil
}

func (s *updateService) rollBack(ctx context.Context, state domain.UpdateState) error {
	old := s.binary + oldSuffix
	if _, err := os.Stat(old); err != nil {
		// Nothing to restore. Recorded as rolled_back anyway, so the daemon
		// stops trying and an operator can see why — looping on a pending update
		// with no fallback is the worst of both.
		state.Status = domain.UpdateRolledBack
		state.UpdatedAt = s.now()
		if putErr := s.tx.InTx(ctx, func(ctx context.Context) error {
			return s.repo.Put(ctx, state)
		}); putErr != nil {
			return putErr
		}
		return fmt.Errorf("update to %s failed to boot %d times and %s is missing, so it cannot be rolled back",
			state.ToVersion, state.BootAttempts, old)
	}

	// The row is written BEFORE the rename, not after.
	//
	// The rename is irreversible; the row is not. Renaming first and failing to
	// persist left the row saying `pending`, which the very next step — Confirm,
	// during the same startup — promoted to `confirmed`. An operator would then
	// see "confirmed 0.1.0 → 0.2.0" on a machine whose binary is 0.1.0, with the
	// fallback deleted. Writing first can at worst mark a rollback that did not
	// happen, which the next boot retries.
	state.Status = domain.UpdateRolledBack
	state.UpdatedAt = s.now()
	if err := s.tx.InTx(ctx, func(ctx context.Context) error {
		return s.repo.Put(ctx, state)
	}); err != nil {
		return fmt.Errorf("record the rollback of %s: %w", state.ToVersion, err)
	}

	if err := os.Rename(old, s.binary); err != nil {
		return fmt.Errorf("restore %s: %w", s.binary, err)
	}
	return nil
}

// Confirm marks a pending update successful and removes the fallback.
//
// Called once the daemon is SERVING, not merely constructed. A binary that
// starts and then fails every request has proven nothing, and deleting .old at
// startup would throw away the rollback while the evidence was still missing.
func (s *updateService) Confirm(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	state, err := s.repo.Get(ctx)
	if err != nil {
		return err
	}
	if state.Status != domain.UpdatePending {
		return nil
	}

	state.Status = domain.UpdateConfirmed
	state.UpdatedAt = s.now()
	if err := s.tx.InTx(ctx, func(ctx context.Context) error {
		return s.repo.Put(ctx, state)
	}); err != nil {
		return err
	}

	// Best-effort, and REPORTED. The update is confirmed either way — the row is
	// already written — so a removal failure must not be returned as an error.
	// But it must not vanish either: a leftover .old is what the next Apply
	// refuses to overwrite, so an operator needs to know it is there.
	//
	// Both arms of this used to return nil, which made the failure
	// indistinguishable from success and the branch impossible to test.
	if err := os.Remove(s.binary + oldSuffix); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: %s could not be removed: %w",
			ErrFallbackNotRemoved, s.binary+oldSuffix, err)
	}
	return nil
}

// BinaryPath is where the daemon's own executable lives.
//
// Resolved through symlinks, because the update replaces a FILE: renaming over a
// symlink would replace the link and leave the real binary untouched, so the
// daemon would relaunch on the old one and loop.
func BinaryPath() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate the running binary: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(self)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", self, err)
	}
	return resolved, nil
}
