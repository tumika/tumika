package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tumika/tumika/source/daemon/internal/domain"
	"github.com/tumika/tumika/source/daemon/internal/platform/buildinfo"
	"github.com/tumika/tumika/source/daemon/internal/platform/release"
	"github.com/tumika/tumika/source/daemon/internal/repository"
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

// ErrNotTheUpdatedBuild means the pending update names a version other than the
// one this process is running, so it is not the update's own binary and cannot
// confirm it.
var ErrNotTheUpdatedBuild = errors.New("the running build is not the one the pending update installed")

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

	// Check reports the daemon component version at the head of the channel
	// this daemon follows, and whether the update rules allow moving to it. It
	// touches nothing.
	Check(ctx context.Context) (available string, newer bool, err error)

	// Apply downloads, verifies, pre-flights and installs a version, then
	// returns. The version must be the one the channel's head ships, and the
	// same rules Check applies decide whether it may be installed.
	//
	// It does NOT exit the process — the caller decides when, because only the
	// caller knows whether a request is still in flight.
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

// UpdateDeps is what the updater is assembled from.
type UpdateDeps struct {
	Repo   repository.UpdateStateRepository
	Source release.Source
	Tx     repository.Txer
	// Settings is narrowed to reading: update.channel is owned by
	// ConfigService, and an updater has no business holding the interface that
	// can also write a setting or read a secret one.
	Settings SettingGetter
	// Schema reports the migration version the database is at. The pre-flight
	// compares it with what the staged binary embeds, so a binary that could
	// not run against this database is refused while the old one is still in
	// charge.
	Schema SchemaVersionFunc
	// Version is the running component's semver; Release is the label of the
	// release that shipped it, which is what its publication time is looked up
	// by.
	Version string
	Release string
	// Binary is the path this daemon's executable lives at, and the path an
	// update replaces.
	Binary string
}

type updateService struct {
	// mu serialises the whole state machine.
	//
	// Apply, ConfirmBoot and Confirm all read the row, act on the filesystem,
	// and write the row back. Two Applies interleaving is not a theoretical
	// race: the scheduled runner and POST /v1/update/apply can both fire, and
	// each is a multi-second download followed by two renames.
	mu   sync.Mutex
	deps UpdateDeps
	// exec runs a pre-flight command on the staged binary and returns its
	// output. Injected so the checks themselves are testable without building a
	// second binary.
	exec func(ctx context.Context, path string, args ...string) (string, error)
	now  func() time.Time
}

// UpdateOption configures the service.
type UpdateOption func(*updateService)

// WithUpdateExec replaces the pre-flight exec.
func WithUpdateExec(f func(ctx context.Context, path string, args ...string) (string, error)) UpdateOption {
	return func(s *updateService) { s.exec = f }
}

// WithUpdateClock replaces the clock.
func WithUpdateClock(f func() time.Time) UpdateOption {
	return func(s *updateService) { s.now = f }
}

// NewUpdateService builds the service.
//
// A nil Schema refuses every update rather than skipping the schema
// pre-flight: an updater that cannot learn what the database is at cannot tell
// whether the binary it is about to install can run against it, and a swap made
// in that ignorance is the one that leaves a daemon unable to start.
func NewUpdateService(deps UpdateDeps, opts ...UpdateOption) UpdateService {
	if deps.Schema == nil {
		deps.Schema = func(context.Context) (int64, error) {
			return 0, errors.New("the database schema version is unavailable")
		}
	}
	s := &updateService{
		deps: deps,
		exec: execVersion,
		now:  func() time.Time { return time.Now().UTC() },
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

func (s *updateService) State(ctx context.Context) (domain.UpdateState, error) {
	return s.deps.Repo.Get(ctx)
}

func (s *updateService) Check(ctx context.Context) (string, bool, error) {
	channel, head, err := s.head(ctx)
	if err != nil {
		return "", false, err
	}
	publishedAt, err := s.publishedAt(ctx)
	if err != nil {
		return "", false, err
	}
	return head.Version, supersedes(channel, head, s.deps.Version, publishedAt), nil
}

// head reads the channel this daemon follows and what that channel offers.
func (s *updateService) head(ctx context.Context) (release.Channel, release.Head, error) {
	name, err := String(ctx, s.deps.Settings, KeyUpdateChannel)
	if err != nil {
		return "", release.Head{}, fmt.Errorf("read the update channel: %w", err)
	}
	channel := release.Channel(name)
	if err := release.ValidateChannel(channel); err != nil {
		return "", release.Head{}, err
	}
	head, err := s.deps.Source.Head(ctx, channel)
	if err != nil {
		return "", release.Head{}, err
	}
	return channel, head, nil
}

// supersedes reports whether a channel's head should replace the running build.
//
// Publication order decides in every channel, because a release label is never
// compared: the head is whatever was published most recently. Stable and beta
// additionally require a greater component version, so a daemon there is never
// walked backwards by a republished older release. Edge asks for recency alone,
// which is what lets it hand out 0.0.2-edge.147 after 0.0.2 — semver ranks a
// prerelease BELOW the version it was cut from, so requiring a greater version
// would strand every edge daemon on the first non-prerelease it saw.
//
// Check and Apply both decide here. Two copies of this comparison is how an
// edge downgrade becomes an update the daemon offers and then refuses to
// install.
func supersedes(channel release.Channel, head release.Head, version string, publishedAt time.Time) bool {
	if !release.Later(head.PublishedAt, publishedAt) {
		return false
	}
	if channel == release.ChannelEdge {
		return true
	}
	return release.Newer(head.Version, version)
}

// publishedAt is when the release this binary was shipped in was published.
//
// A development build is dated at the zero time — older than anything a channel
// offers, so it is offered the head rather than pinned to nothing. When the
// running build's own bill of materials is not published (edge releases are
// pruned) the WATERMARK stands in for it, and only a daemon with no watermark
// falls back to the zero time. Any other failure is returned instead: a
// signature that does not verify, or a host that is down, says nothing about
// recency, and reading it as "older" would hand an edge daemon a DOWNGRADE on
// the strength of a document nobody could verify.
//
// A bill of materials that reads cleanly is authoritative and the watermark is
// left alone: the watermark is a floor for a document that has gone, and
// raising it would make Check — which a runner calls on a timer and which
// otherwise touches nothing — a writer of the state row.
func (s *updateService) publishedAt(ctx context.Context) (time.Time, error) {
	if s.deps.Release == "" || s.deps.Release == buildinfo.DevRelease {
		return time.Time{}, nil
	}
	bom, err := s.deps.Source.ReleaseBOM(ctx, s.deps.Release)
	if err != nil {
		if errors.Is(err, release.ErrNoRelease) {
			return s.watermark(ctx)
		}
		return time.Time{}, fmt.Errorf("read the bill of materials of release %s: %w", s.deps.Release, err)
	}
	return bom.PublishedAt, nil
}

// watermark is the publication time this daemon recorded for the build it is
// running, and the zero time when it has none.
//
// It is what stops a rollback by omission. Recency alone decides on edge, so a
// host that serves a 404 for the running build's own document — which is
// indistinguishable from a legitimate prune — dates the daemon at nothing, and
// a genuine, correctly signed, older channel head then supersedes it. No
// signature has to be forged; withholding one document is enough. The watermark
// is written from the release being installed, by this daemon, before the
// binary is swapped, so no reply from the host can lower it.
//
// It counts only while it belongs to the running build: the row's ToVersion
// must be the running component version, and its status must be pending or
// confirmed. A rolled_back or failed row records a build that is NOT running,
// and its publication time would date this daemon by a release it never kept.
//
// The state is what that gate is read from, and the watermark is read through
// its own repository method only once the gate has passed. Reaching it means a
// daemon that is serving, so the migrations have run and the column is there.
func (s *updateService) watermark(ctx context.Context) (time.Time, error) {
	state, err := s.deps.Repo.Get(ctx)
	if err != nil {
		return time.Time{}, fmt.Errorf("read the update state: %w", err)
	}
	if state.ToVersion != s.deps.Version {
		return time.Time{}, nil
	}
	if state.Status != domain.UpdatePending && state.Status != domain.UpdateConfirmed {
		return time.Time{}, nil
	}
	at, err := s.deps.Repo.Watermark(ctx)
	if err != nil {
		return time.Time{}, fmt.Errorf("read the update watermark: %w", err)
	}
	if at == nil {
		return time.Time{}, nil
	}
	return *at, nil
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

	// The bytes come from the channel's head and nowhere else, so the version
	// asked for has to be the one it ships.
	//
	// The head can move between a check and an apply. Installing whatever the
	// channel now offers would install a release nothing approved, and refusing
	// leaves the next check to offer the new head on its own terms.
	channel, head, err := s.head(ctx)
	if err != nil {
		return err
	}
	if head.Version != version {
		return fmt.Errorf("%w: the %s head %s ships %s, not %s",
			domain.ErrConflict, channel, head.Release, head.Version, version)
	}

	publishedAt, err := s.publishedAt(ctx)
	if err != nil {
		return err
	}
	if !supersedes(channel, head, s.deps.Version, publishedAt) {
		return fmt.Errorf("%w: %s from release %s does not supersede the running %s on the %s channel",
			domain.ErrConflict, head.Version, head.Release, s.deps.Version, channel)
	}

	// REFUSE when an update is already installed and waiting for its restart.
	//
	// This process still reports the OLD version — it is still executing the old
	// binary — so the head comparison above stays true after a successful apply,
	// and a second Apply would sail straight through. It would then move the NEW
	// binary to .old and install the new one again, so .old would hold the
	// broken version and a rollback would "restore" it. The daemon would
	// crash-loop with no way back.
	//
	// Reachable from an operator running `tumika update` twice, from two
	// concurrent POSTs, and from the runner racing the API — all of which end at
	// the same destroyed fallback.
	current, err := s.deps.Repo.Get(ctx)
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
	// 0600 file CreateTemp makes would have the download write into it and the
	// pre-flight then fail to execute it — the mode of an existing file is not
	// changed by a write.
	stagedFile, err := os.CreateTemp(filepath.Dir(s.deps.Binary), ".tumika-staged-*")
	if err != nil {
		return fmt.Errorf("stage an update next to %s: %w", s.deps.Binary, err)
	}
	staged := stagedFile.Name()
	_ = stagedFile.Close()
	_ = os.Remove(staged)
	defer func() { _ = os.Remove(staged) }()

	if err := s.deps.Source.FetchAsset(ctx, head.Asset, staged); err != nil {
		return err
	}

	// PRE-FLIGHT, while the old binary is still in charge.
	//
	// The checksum proves the bytes are the ones published. It does not prove
	// they run: a build for the wrong architecture, or one linked against a libc
	// this host does not have, hashes perfectly and cannot execute. Finding that
	// out AFTER the replacement means a daemon that exits 203 forever.
	if err := s.preflight(ctx, staged, version); err != nil {
		return err
	}

	// Recorded BEFORE the replacement. A crash between this write and the
	// rename leaves `pending` against a binary that was never swapped — which
	// ConfirmBoot resolves harmlessly. The reverse order leaves a swapped binary
	// with no record, and nothing would ever roll it back.
	started := s.now()
	state := domain.UpdateState{
		Status:       domain.UpdatePending,
		FromVersion:  s.deps.Version,
		ToVersion:    version,
		BootAttempts: 0,
		StartedAt:    &started,
		UpdatedAt:    started,
	}
	// The head's publication time is recorded with the version it ships, in the
	// same transaction, so the binary that boots next can date itself even after
	// its own bill of materials stops being served — which is what keeps a 404
	// from becoming a downgrade. One transaction because a watermark that names
	// a version the row does not is a floor belonging to nothing.
	headPublishedAt := head.PublishedAt
	if err := s.deps.Tx.InTx(ctx, func(ctx context.Context) error {
		if err := s.deps.Repo.Put(ctx, state); err != nil {
			return err
		}
		return s.deps.Repo.PutWatermark(ctx, &headPublishedAt)
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
	if _, statErr := os.Stat(s.deps.Binary + oldSuffix); statErr == nil {
		return fmt.Errorf("%w: %s already exists but no update is pending; "+
			"remove it once you are satisfied the running version is good",
			domain.ErrConflict, s.deps.Binary+oldSuffix)
	}
	if err := os.Rename(s.deps.Binary, s.deps.Binary+oldSuffix); err != nil {
		return fmt.Errorf("keep the current binary as %s: %w", s.deps.Binary+oldSuffix, err)
	}
	if err := os.Rename(staged, s.deps.Binary); err != nil {
		// Put it back. Failing here with neither binary in place would leave
		// nothing for the supervisor to start.
		if restoreErr := os.Rename(s.deps.Binary+oldSuffix, s.deps.Binary); restoreErr != nil {
			return fmt.Errorf("install %s: %w (and restoring the previous binary failed: %v)",
				s.deps.Binary, err, restoreErr)
		}
		return fmt.Errorf("install %s: %w", s.deps.Binary, err)
	}
	return nil
}

// preflight runs the staged binary twice and judges what it says about itself.
//
// Both answers are refusals the old binary is still in a position to act on:
//
//   - a binary that reports a different version is not the release that was
//     asked for, so the daemon would go on claiming a version it is not and the
//     next comparison would be made against a lie;
//   - a binary embedding FEWER migrations than the database has applied cannot
//     run against this database at all. ConfirmBoot runs before Migrate, so
//     finding that out afterwards is three failed boots and a rollback instead
//     of one refusal here.
func (s *updateService) preflight(ctx context.Context, path, version string) error {
	ctx, cancel := context.WithTimeout(ctx, preflightTimeout)
	defer cancel()

	out, err := s.exec(ctx, path, "version")
	if err != nil {
		return fmt.Errorf("%w: the downloaded binary could not be run: %w",
			domain.ErrConflict, err)
	}
	reported, err := parseVersion(out)
	if err != nil {
		return err
	}
	if reported != version {
		return fmt.Errorf("%w: the downloaded binary reports version %q, not %q",
			domain.ErrConflict, reported, version)
	}

	applied, err := s.deps.Schema(ctx)
	if err != nil {
		return fmt.Errorf("%w: the database schema version could not be read: %w",
			domain.ErrConflict, err)
	}
	jsonOut, err := s.exec(ctx, path, "version", "--json")
	if err != nil {
		return fmt.Errorf("%w: the downloaded binary could not report its build information: %w",
			domain.ErrConflict, err)
	}
	embedded, err := parseSchemaVersion(jsonOut)
	if err != nil {
		return err
	}
	if embedded < applied {
		return fmt.Errorf("%w: %s embeds database schema %d and this database is at %d, "+
			"so it cannot run against it",
			domain.ErrConflict, version, embedded, applied)
	}
	return nil
}

// execVersion runs `<path> version` with the arguments the pre-flight asks for
// and returns its output.
func execVersion(ctx context.Context, path string, args ...string) (string, error) {
	// Both scanners flag the variable command. The audit: path is always the
	// staged file Apply just created next to tumika's own binary — it is not
	// caller-supplied, and every argument is a literal from the pre-flight.
	// Running it is the entire point of a pre-flight.
	//
	// nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
	cmd := exec.CommandContext(ctx, path, args...) // #nosec G204 -- see the audit above: path is the file Apply staged beside our own binary
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

// parseSchemaVersion reads the embedded migration version out of
// `tumika version --json`.
//
// The document is the binary's own build identity, so it is decoded into the
// type that produced it rather than a local struct that could drift from it.
func parseSchemaVersion(out string) (int64, error) {
	var info buildinfo.Info
	if err := json.Unmarshal([]byte(out), &info); err != nil {
		return 0, fmt.Errorf("%w: unreadable `tumika version --json` output: %w",
			domain.ErrConflict, err)
	}
	return info.SchemaVersion, nil
}

// ConfirmBoot resolves whatever the previous process left behind.
//
// Called at startup, BEFORE serving. The counter is incremented first and read
// afterwards: a binary that crashes during startup must still have its attempt
// counted, or it would loop forever without ever reaching the rollback.
func (s *updateService) ConfirmBoot(ctx context.Context) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	state, err := s.deps.Repo.Get(ctx)
	if err != nil {
		return false, err
	}
	if state.Status != domain.UpdatePending {
		return false, nil
	}

	state, err = s.deps.Repo.IncrementBootAttempts(ctx)
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
	old := s.deps.Binary + oldSuffix
	if _, err := os.Stat(old); err != nil {
		// Nothing to restore. Recorded as rolled_back anyway, so the daemon
		// stops trying and an operator can see why — looping on a pending update
		// with no fallback is the worst of both.
		state.Status = domain.UpdateRolledBack
		state.UpdatedAt = s.now()
		if putErr := s.deps.Tx.InTx(ctx, func(ctx context.Context) error {
			return s.deps.Repo.Put(ctx, state)
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
	if err := s.deps.Tx.InTx(ctx, func(ctx context.Context) error {
		return s.deps.Repo.Put(ctx, state)
	}); err != nil {
		return fmt.Errorf("record the rollback of %s: %w", state.ToVersion, err)
	}

	if err := os.Rename(old, s.deps.Binary); err != nil {
		return fmt.Errorf("restore %s: %w", s.deps.Binary, err)
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

	state, err := s.deps.Repo.Get(ctx)
	if err != nil {
		return err
	}
	if state.Status != domain.UpdatePending {
		return nil
	}

	// The row is evidence about ONE binary — the one it names. A process whose
	// component version differs is not that binary, however well it serves: the
	// swap may have been undone, or the supervisor may have relaunched a
	// different unit. Confirming from here would record a version nobody is
	// running as proven and delete the .old that is still the way back from the
	// one that is. The row stays pending, so the binary it names gets its boot
	// attempts and its rollback when it next starts.
	if state.ToVersion != s.deps.Version {
		return fmt.Errorf("%w: the pending update installed %s and this build is %s",
			ErrNotTheUpdatedBuild, state.ToVersion, s.deps.Version)
	}

	state.Status = domain.UpdateConfirmed
	state.UpdatedAt = s.now()
	if err := s.deps.Tx.InTx(ctx, func(ctx context.Context) error {
		return s.deps.Repo.Put(ctx, state)
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
	if err := os.Remove(s.deps.Binary + oldSuffix); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: %s could not be removed: %w",
			ErrFallbackNotRemoved, s.deps.Binary+oldSuffix, err)
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
