package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tumika/tumika/source/daemon/internal/domain"
	"github.com/tumika/tumika/source/daemon/internal/platform/buildinfo"
	"github.com/tumika/tumika/source/daemon/internal/platform/release"
	"github.com/tumika/tumika/source/daemon/internal/service"
)

// published is the instant the running build's own release is dated at. Heads
// that are meant to supersede it are offset forward.
var published = time.Date(2026, 9, 20, 7, 28, 0, 0, time.UTC)

// runningRelease is the label the harness's daemon was shipped in, and the
// label it looks itself up under.
const runningRelease = "2026.09.00"

// fakeSource is a release host the test controls: one head per channel, the
// bills of materials of the releases it has published, and the bytes each
// asset downloads as.
type fakeSource struct {
	heads   map[release.Channel]release.Head
	headErr error
	// boms answers ReleaseBOM. A label that is absent is a release nobody
	// published, or one that has been pruned: ErrNoRelease.
	boms   map[string]*release.BOM
	bomErr error
	// bodies is what FetchAsset writes, keyed by asset URL.
	bodies   map[string]string
	fetchErr error
	fetched  []string
}

func newFakeSource() *fakeSource {
	return &fakeSource{
		heads:  map[release.Channel]release.Head{},
		boms:   map[string]*release.BOM{},
		bodies: map[string]string{},
	}
}

// offer publishes a release: it becomes the channel's head, it can be looked
// up by label, and its asset downloads as a binary reporting that version.
func (f *fakeSource) offer(channel release.Channel, label string, at time.Time, version string) {
	url := "https://releases.test/" + label + "/tumika"
	f.heads[channel] = release.Head{
		Release:     label,
		Channel:     channel,
		PublishedAt: at,
		Version:     version,
		Asset:       release.Asset{URL: url, SHA256: strings.Repeat("0", 64)},
	}
	f.bodies[url] = "binary " + version
	f.publish(label, channel, at, version)
}

// publish records a release's own bill of materials without making it a head.
func (f *fakeSource) publish(label string, channel release.Channel, at time.Time, version string) {
	f.boms[label] = &release.BOM{
		Release:     label,
		Channel:     channel,
		PublishedAt: at,
		Components: map[string]release.Component{
			release.DaemonComponent: {Version: version},
		},
	}
}

// serve replaces the bytes a channel head's asset downloads as.
func (f *fakeSource) serve(channel release.Channel, body string) {
	f.bodies[f.heads[channel].Asset.URL] = body
}

func (f *fakeSource) Head(_ context.Context, channel release.Channel) (release.Head, error) {
	if f.headErr != nil {
		return release.Head{}, f.headErr
	}
	head, ok := f.heads[channel]
	if !ok {
		return release.Head{}, release.ErrNoRelease
	}
	return head, nil
}

func (f *fakeSource) ReleaseBOM(_ context.Context, label string) (*release.BOM, error) {
	if f.bomErr != nil {
		return nil, f.bomErr
	}
	bom, ok := f.boms[label]
	if !ok {
		return nil, release.ErrNoRelease
	}
	return bom, nil
}

func (f *fakeSource) FetchAsset(_ context.Context, asset release.Asset, dest string) error {
	f.fetched = append(f.fetched, asset.URL)
	if f.fetchErr != nil {
		return f.fetchErr
	}
	body, ok := f.bodies[asset.URL]
	if !ok {
		body = "binary"
	}
	if err := os.WriteFile(dest, []byte(body), 0o755); err != nil { //nolint:gosec // a stand-in for an executable
		return err
	}
	// Chmod explicitly, because WriteFile does NOT apply its mode to a file that
	// already exists — and the real FetchAsset guarantees an executable file at
	// dest. Without this the fake is more permissive than production in one
	// direction and less in another, which is how a fake stops testing anything.
	return os.Chmod(dest, 0o755) //nolint:gosec // a stand-in for an executable
}

var _ release.Source = (*fakeSource)(nil)

// fakeSettings answers the one setting the updater reads.
type fakeSettings struct {
	channel release.Channel
	err     error
}

func (f fakeSettings) Get(_ context.Context, key string) (domain.SettingView, error) {
	if f.err != nil {
		return domain.SettingView{}, f.err
	}
	channel := f.channel
	if channel == "" {
		channel = release.ChannelStable
	}
	return domain.SettingView{Key: key, Value: json.RawMessage(`"` + string(channel) + `"`)}, nil
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
	svc      service.UpdateService
	repo     *fakeUpdateRepo
	source   *fakeSource
	settings *fakeSettings
	binary   string
	// preflight is what the injected exec reports as the staged binary's
	// version. Empty means "match whatever was staged", which is the healthy
	// case.
	preflight    string
	preflightErr error
	// dbSchema is the migration version the database is at, and stagedSchema
	// what the staged binary reports embedding.
	dbSchema     int64
	stagedSchema int64
}

func newHarness(t *testing.T, current string, opts ...service.UpdateOption) *harness {
	t.Helper()

	dir := t.TempDir()
	binary := filepath.Join(dir, "tumika")
	if err := os.WriteFile(binary, []byte("binary "+current), 0o755); err != nil { //nolint:gosec // a stand-in for an executable
		t.Fatalf("write: %v", err)
	}

	h := &harness{
		repo:         newUpdateRepo(),
		source:       newFakeSource(),
		settings:     &fakeSettings{},
		binary:       binary,
		dbSchema:     7,
		stagedSchema: 7,
	}
	// The running build's own release, and a later stable head offering 0.2.0 —
	// the ordinary state a check finds.
	h.source.publish(runningRelease, release.ChannelStable, published, current)
	h.source.offer(release.ChannelStable, "2026.09.01", published.Add(24*time.Hour), "0.2.0")

	base := []service.UpdateOption{
		service.WithUpdateExec(func(_ context.Context, path string, args ...string) (string, error) {
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
			if slices.Contains(args, "--json") {
				return fmt.Sprintf(`{"version":%q,"release":%q,"schema_version":%d}`,
					reported, "2026.09.01", h.stagedSchema), nil
			}
			return "tumika " + reported +
				" (release 2026.09.01, commit abc, built now, go1.26.6, linux/arm64)\n", nil
		}),
	}
	h.svc = service.NewUpdateService(service.UpdateDeps{
		Repo:     h.repo,
		Source:   h.source,
		Tx:       &fakeTxer{},
		Settings: h.settings,
		Schema:   func(context.Context) (int64, error) { return h.dbSchema, nil },
		Version:  current,
		Release:  runningRelease,
		Binary:   binary,
	}, append(base, opts...)...)
	return h
}

// directDeps is the updater's dependencies for the tests that build a service
// themselves rather than through the harness, over a stable channel offering
// 0.2.0 in a release published after the running build's own.
func directDeps(repo *fakeUpdateRepo, current, binary string) service.UpdateDeps {
	source := newFakeSource()
	source.publish(runningRelease, release.ChannelStable, published, current)
	source.offer(release.ChannelStable, "2026.09.01", published.Add(24*time.Hour), "0.2.0")
	return service.UpdateDeps{
		Repo:     repo,
		Source:   source,
		Tx:       &fakeTxer{},
		Settings: &fakeSettings{},
		Schema:   func(context.Context) (int64, error) { return 1, nil },
		Version:  current,
		Release:  runningRelease,
		Binary:   binary,
	}
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

// A binary embedding fewer migrations than the database has applied cannot run
// against this database. The refusal happens in the pre-flight, while the old
// binary is still in charge: ConfirmBoot runs BEFORE Migrate, so a schema
// refusal afterwards is a failed boot, and three of those roll back.
func TestApplyRefusesABinaryEmbeddingAnOlderSchemaThanTheDatabase(t *testing.T) {
	h := newHarness(t, "0.1.0")
	h.dbSchema = 9
	h.stagedSchema = 8

	err := h.svc.Apply(t.Context(), "0.2.0")
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("= %v, want ErrConflict", err)
	}
	if !strings.Contains(err.Error(), "8") || !strings.Contains(err.Error(), "9") {
		t.Errorf("the error does not say which schema is which: %v", err)
	}

	// Nothing was replaced, nothing was set aside, and nothing was recorded.
	if got := h.read(t, h.binary); got != "binary 0.1.0" {
		t.Errorf("the running binary is %q; it was replaced", got)
	}
	if _, statErr := os.Stat(h.binary + ".old"); statErr == nil {
		t.Error("a rollback copy was made for an update that was refused")
	}
	if h.repo.state.Status != domain.UpdateIdle {
		t.Errorf("status = %q, want idle — nothing was installed", h.repo.state.Status)
	}
}

// The build information is the only evidence of what the staged binary embeds,
// so output that cannot be read is refused rather than read optimistically —
// an unreadable document must never pass for "schema 0, which is fine".
func TestApplyRefusesUnreadableBuildInformation(t *testing.T) {
	for _, output := range []string{"", "Segmentation fault", "{"} {
		h := newHarness(t, "0.1.0", service.WithUpdateExec(
			func(_ context.Context, _ string, args ...string) (string, error) {
				if slices.Contains(args, "--json") {
					return output, nil
				}
				return "tumika 0.2.0 (release 2026.09.01, commit abc, built now, go1.26.6, linux/arm64)\n", nil
			}))
		h.dbSchema = 9

		if err := h.svc.Apply(t.Context(), "0.2.0"); !errors.Is(err, domain.ErrConflict) {
			t.Errorf("--json output %q = %v, want ErrConflict", output, err)
		}
		if got := h.read(t, h.binary); got != "binary 0.1.0" {
			t.Errorf("--json output %q: the running binary was replaced", output)
		}
	}
}

// A binary embedding MORE migrations is the ordinary update: it migrates the
// database forward once it boots.
func TestApplyAcceptsABinaryEmbeddingANewerSchema(t *testing.T) {
	h := newHarness(t, "0.1.0")
	h.dbSchema = 9
	h.stagedSchema = 10

	if err := h.svc.Apply(t.Context(), "0.2.0"); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := h.read(t, h.binary); got != "binary 0.2.0" {
		t.Errorf("the binary is %q, want the new one", got)
	}
}

// An updater that cannot learn what the database is at cannot tell whether the
// binary it is about to install can run against it, so it refuses rather than
// swapping in ignorance.
func TestApplyRefusesWhenTheDatabaseSchemaCannotBeRead(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "tumika")
	if err := os.WriteFile(binary, []byte("binary 0.1.0"), 0o755); err != nil { //nolint:gosec // a stand-in for an executable
		t.Fatalf("write: %v", err)
	}

	for name, deps := range map[string]service.UpdateDeps{
		"unreadable": func() service.UpdateDeps {
			d := directDeps(newUpdateRepo(), "0.1.0", binary)
			d.Schema = func(context.Context) (int64, error) {
				return 0, errors.New("database is locked")
			}
			return d
		}(),
		"absent": func() service.UpdateDeps {
			d := directDeps(newUpdateRepo(), "0.1.0", binary)
			d.Schema = nil
			return d
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			svc := service.NewUpdateService(deps)
			if err := svc.Apply(t.Context(), "0.2.0"); !errors.Is(err, domain.ErrConflict) {
				t.Fatalf("= %v, want ErrConflict", err)
			}
			body, readErr := os.ReadFile(binary) //nolint:gosec // a path this test created
			if readErr != nil {
				t.Fatalf("read: %v", readErr)
			}
			if string(body) != "binary 0.1.0" {
				t.Errorf("the running binary is %q; it was replaced", body)
			}
		})
	}
}

// The bytes come from the channel's head, so a version the head does not ship
// cannot be installed. The head moves between a check and an apply, and
// installing whatever the channel now offers would install a release nothing
// approved.
func TestApplyRefusesAVersionTheHeadDoesNotShip(t *testing.T) {
	h := newHarness(t, "0.1.0")

	for _, version := range []string{"0.1.0", "0.1.9", "0.3.0"} {
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

func TestCheckReportsWhatTheHeadOffers(t *testing.T) {
	h := newHarness(t, "0.1.0")

	available, newer, err := h.svc.Check(t.Context())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if available != "0.2.0" || !newer {
		t.Errorf("Check = %q/%v, want 0.2.0/true", available, newer)
	}

	// The head is the release this daemon is already running: nothing to do,
	// and the version is still reported so a caller can show it.
	h.source.offer(release.ChannelStable, runningRelease, published, "0.1.0")
	available, newer, err = h.svc.Check(t.Context())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if available != "0.1.0" || newer {
		t.Errorf("Check = %q/%v, want 0.1.0/false", available, newer)
	}
}

// The rule, for every channel and every way a head can relate to the running
// build. Check offers exactly what Apply will install: a second copy of this
// comparison is how an edge downgrade becomes an update the daemon offers and
// then refuses.
func TestTheUpdateRulePerChannel(t *testing.T) {
	const running = "0.2.0"

	for _, tc := range []struct {
		name    string
		channel release.Channel
		// later says the head was published after the running build's release;
		// version is what the head ships.
		later   bool
		version string
		want    bool
	}{
		{"stable later and greater", release.ChannelStable, true, "0.3.0", true},
		{"stable later but lower", release.ChannelStable, true, "0.1.0", false},
		{"stable earlier and greater", release.ChannelStable, false, "0.3.0", false},
		{"stable earlier and lower", release.ChannelStable, false, "0.1.0", false},

		{"beta later and greater", release.ChannelBeta, true, "0.3.0", true},
		{"beta later but lower", release.ChannelBeta, true, "0.1.0", false},
		{"beta earlier and greater", release.ChannelBeta, false, "0.3.0", false},
		{"beta earlier and lower", release.ChannelBeta, false, "0.1.0", false},

		// Edge compares recency alone. Semver ranks 0.0.2-edge.147 below the
		// 0.0.2 it was cut from, so requiring a greater version would strand
		// every edge daemon on the first non-prerelease it saw.
		{"edge later and greater", release.ChannelEdge, true, "0.3.0", true},
		{"edge later but lower", release.ChannelEdge, true, "0.1.0", true},
		{"edge earlier and greater", release.ChannelEdge, false, "0.3.0", false},
		{"edge earlier and lower", release.ChannelEdge, false, "0.1.0", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, running)
			h.settings.channel = tc.channel

			at := published.Add(-24 * time.Hour)
			if tc.later {
				at = published.Add(24 * time.Hour)
			}
			label := "2026.09.02"
			if tc.channel == release.ChannelEdge {
				label = "edge.147"
			}
			h.source.offer(tc.channel, label, at, tc.version)

			available, newer, err := h.svc.Check(t.Context())
			if err != nil {
				t.Fatalf("Check: %v", err)
			}
			if available != tc.version {
				t.Errorf("Check offered %q, want %q", available, tc.version)
			}
			if newer != tc.want {
				t.Errorf("Check newer = %v, want %v", newer, tc.want)
			}

			// Apply decides by the same rule, so it accepts exactly what Check
			// offered.
			err = h.svc.Apply(t.Context(), tc.version)
			switch {
			case tc.want && err != nil:
				t.Errorf("Apply refused what Check offered: %v", err)
			case !tc.want && !errors.Is(err, domain.ErrConflict):
				t.Errorf("Apply = %v, want ErrConflict for a head Check did not offer", err)
			}
		})
	}
}

// A daemon whose own release has been pruned — every edge release is, in time —
// and which carries no watermark for the build it is running is dated at the
// zero time, so the head is offered rather than the daemon being pinned to
// nothing.
func TestAPrunedOwnReleaseCountsAsOlderThanTheHead(t *testing.T) {
	h := newHarness(t, "0.0.2-edge.140")
	h.settings.channel = release.ChannelEdge
	delete(h.source.boms, runningRelease)
	h.source.offer(release.ChannelEdge, "edge.147", published.Add(-365*24*time.Hour), "0.0.2-edge.147")

	if _, newer, err := h.svc.Check(t.Context()); err != nil || !newer {
		t.Errorf("Check = %v/%v, want an offer: a pruned release dates the daemon at nothing", newer, err)
	}
}

// THE rollback a hostile release host gets for free without forging anything.
//
// Edge decides on recency alone, and a 404 on the running build's own document
// is indistinguishable from a legitimate prune. So a host that withholds that
// one document and replays a genuine, correctly signed, OLDER channel head
// would walk the daemon backwards — the signature chain is intact throughout.
// The watermark this daemon wrote when it installed the build it is running is
// the floor that refuses it.
func TestAWatermarkRefusesAReplayedOlderHeadWhenTheOwnDocumentIsWithheld(t *testing.T) {
	const running = "0.0.2-edge.140"
	replayed := published.Add(-48 * time.Hour)

	withheld := func(t *testing.T) *harness {
		t.Helper()
		h := newHarness(t, running)
		h.settings.channel = release.ChannelEdge
		h.source.offer(release.ChannelEdge, "edge.100", replayed, "0.0.2-edge.100")
		// The host serves the replayed head and its document, and withholds the
		// running build's own.
		delete(h.source.boms, runningRelease)
		return h
	}

	t.Run("with a watermark", func(t *testing.T) {
		h := withheld(t)
		h.repo.state = domain.UpdateState{
			Status: domain.UpdateConfirmed, FromVersion: "0.0.2-edge.139", ToVersion: running,
			ToPublishedAt: &published,
		}

		available, newer, err := h.svc.Check(t.Context())
		if err != nil {
			t.Fatalf("Check: %v", err)
		}
		if available != "0.0.2-edge.100" {
			t.Errorf("Check offered %q, want the head's version reported either way", available)
		}
		if newer {
			t.Error("a replayed older head superseded a build with a newer watermark")
		}

		if err := h.svc.Apply(t.Context(), "0.0.2-edge.100"); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("Apply = %v, want ErrConflict", err)
		}
		if got := h.read(t, h.binary); got != "binary "+running {
			t.Errorf("the running binary is %q; it was rolled backwards", got)
		}
	})

	// The same host, the same replay, against a daemon that has never updated:
	// it has nothing to be dated by, so it takes the head. That is the case the
	// watermark must not break, because it is every first install.
	t.Run("without a watermark", func(t *testing.T) {
		h := withheld(t)

		if _, newer, err := h.svc.Check(t.Context()); err != nil || !newer {
			t.Errorf("Check = %v/%v, want the head offered to a daemon with no watermark", newer, err)
		}
	})
}

// A watermark counts only while it belongs to the build that is running. A
// rolled_back row records a version this daemon does NOT run, so dating the
// daemon by it would pin it to a release it never kept.
func TestAWatermarkFromAnotherBuildIsIgnored(t *testing.T) {
	const running = "0.0.2-edge.140"

	for _, tc := range []struct {
		name  string
		state domain.UpdateState
	}{
		{"rolled back", domain.UpdateState{
			Status: domain.UpdateRolledBack, FromVersion: running, ToVersion: "0.0.2-edge.150",
			ToPublishedAt: &published,
		}},
		{"pending another version", domain.UpdateState{
			Status: domain.UpdatePending, FromVersion: running, ToVersion: "0.0.2-edge.150",
			ToPublishedAt: &published,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, running)
			h.settings.channel = release.ChannelEdge
			h.source.offer(release.ChannelEdge, "edge.100", published.Add(-48*time.Hour), "0.0.2-edge.100")
			delete(h.source.boms, runningRelease)
			h.repo.state = tc.state

			if _, newer, err := h.svc.Check(t.Context()); err != nil || !newer {
				t.Errorf("Check = %v/%v, want the head offered: the watermark is another build's",
					newer, err)
			}
		})
	}
}

// The running build's own bill of materials is the authority whenever it can be
// read; the watermark only stands in for one that is gone.
func TestTheOwnBillOfMaterialsWinsOverTheWatermark(t *testing.T) {
	const running = "0.0.2-edge.140"
	h := newHarness(t, running)
	h.settings.channel = release.ChannelEdge
	// The daemon's own release is dated a year ago, and the head is newer than
	// that but older than the watermark. Only the document decides.
	h.source.publish(runningRelease, release.ChannelEdge, published.Add(-365*24*time.Hour), running)
	h.source.offer(release.ChannelEdge, "edge.147", published.Add(-48*time.Hour), "0.0.2-edge.147")
	h.repo.state = domain.UpdateState{
		Status: domain.UpdateConfirmed, ToVersion: running, ToPublishedAt: &published,
	}

	if _, newer, err := h.svc.Check(t.Context()); err != nil || !newer {
		t.Errorf("Check = %v/%v, want the head offered on the strength of the own document",
			newer, err)
	}
}

// The watermark is written with the pending row, before the binary is swapped,
// so the build that boots next can date itself without asking the host.
func TestApplyRecordsThePublicationTimeOfWhatItInstalls(t *testing.T) {
	h := newHarness(t, "0.1.0")

	if err := h.svc.Apply(t.Context(), "0.2.0"); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	state, err := h.repo.Get(t.Context())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	want := published.Add(24 * time.Hour)
	if state.ToPublishedAt == nil || !state.ToPublishedAt.Equal(want) {
		t.Errorf("ToPublishedAt = %v, want the head's publication time %v", state.ToPublishedAt, want)
	}
}

// A bill of materials that will not verify, or a host that is down, says
// NOTHING about when the running build was published. Reading that silence as
// "older" hands an edge daemon a downgrade on the strength of a document nobody
// could verify, so the check fails loudly instead.
func TestAnUnreadableOwnBOMStopsTheCheckRatherThanDowngrading(t *testing.T) {
	h := newHarness(t, "0.2.0")
	h.settings.channel = release.ChannelEdge
	h.source.offer(release.ChannelEdge, "edge.147", published.Add(24*time.Hour), "0.1.0")
	h.source.bomErr = release.ErrBadSignature

	if _, newer, err := h.svc.Check(t.Context()); err == nil {
		t.Fatal("an unverifiable own bill of materials was treated as an answer")
	} else if newer {
		t.Error("a failed lookup offered a downgrade")
	}

	if err := h.svc.Apply(t.Context(), "0.1.0"); err == nil {
		t.Fatal("Apply installed a downgrade it could not date the running build against")
	}
	if got := h.read(t, h.binary); got != "binary 0.2.0" {
		t.Errorf("the running binary is %q; it was replaced", got)
	}
}

// A development build has no bill of materials to look itself up in, and asking
// for one would put "dev" in a URL path.
func TestADevReleaseIsNeverLookedUp(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "tumika")
	if err := os.WriteFile(binary, []byte("binary 0.1.0"), 0o755); err != nil { //nolint:gosec // a stand-in for an executable
		t.Fatalf("write: %v", err)
	}

	source := newFakeSource()
	source.offer(release.ChannelStable, "2026.09.01", published, "0.2.0")
	// Anything that reaches ReleaseBOM fails the test by failing the check.
	source.bomErr = errors.New("a development build asked for its own release")

	svc := service.NewUpdateService(service.UpdateDeps{
		Repo:     newUpdateRepo(),
		Source:   source,
		Tx:       &fakeTxer{},
		Settings: &fakeSettings{},
		Schema:   func(context.Context) (int64, error) { return 0, nil },
		Version:  "0.1.0",
		Release:  buildinfo.DevRelease,
		Binary:   binary,
	})

	if _, newer, err := svc.Check(t.Context()); err != nil || !newer {
		t.Errorf("Check = %v/%v, want the head offered without a lookup", newer, err)
	}
}

// An unknown channel is refused rather than placed in a URL: the value comes
// from the settings table, which an operator writes.
func TestCheckRefusesAChannelThatIsNotOne(t *testing.T) {
	h := newHarness(t, "0.1.0")
	h.settings.channel = "nightly"

	if _, _, err := h.svc.Check(t.Context()); !errors.Is(err, release.ErrInvalidChannel) {
		t.Fatalf("= %v, want ErrInvalidChannel", err)
	}
}

// A settings table that cannot be read is a failure, not a silent fallback to
// stable: a daemon an operator moved to beta must not quietly follow another
// channel.
func TestCheckReportsAnUnreadableChannelSetting(t *testing.T) {
	h := newHarness(t, "0.1.0")
	h.settings.err = errors.New("database is locked")

	if _, newer, err := h.svc.Check(t.Context()); err == nil {
		t.Fatal("an unreadable channel setting reported success")
	} else if newer {
		t.Error("an unreadable channel setting offered an update")
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
	h.source.headErr = errors.New("connection reset")

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
			func(context.Context, string, ...string) (string, error) { return output, nil }))

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
	svc := service.NewUpdateService(directDeps(repo, "0.1.0", binary),
		service.WithUpdateExec(func(_ context.Context, _ string, args ...string) (string, error) {
			_ = os.Remove(binary)
			if slices.Contains(args, "--json") {
				return `{"version":"0.2.0","schema_version":9}`, nil
			}
			return "tumika 0.2.0 (release 2026.09.01, commit abc, built now, go1.26.6, linux/arm64)\n", nil
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

	// What the "downloaded" binary will be: a script answering both pre-flight
	// questions — the version it reports, and the schema it embeds.
	script := "#!/bin/sh\n" +
		"case \"$2\" in\n" +
		"  --json) echo '{\"version\":\"0.2.0\",\"schema_version\":9}' ;;\n" +
		"  *) echo 'tumika 0.2.0 (release 2026.09.01, commit abc, built now, go1.26.6, linux/arm64)' ;;\n" +
		"esac\n"

	deps := directDeps(newUpdateRepo(), "0.1.0", binary)
	deps.Source.(*fakeSource).serve(release.ChannelStable, script)
	svc := service.NewUpdateService(deps)

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

	deps := directDeps(newUpdateRepo(), "0.1.0", binary)
	deps.Source.(*fakeSource).serve(release.ChannelStable, "not a program at all")
	svc := service.NewUpdateService(deps)

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
	svc := service.NewUpdateService(directDeps(repo, "0.2.0", binary))

	err := svc.Confirm(t.Context())
	if !errors.Is(err, service.ErrFallbackNotRemoved) {
		t.Fatalf("= %v, want ErrFallbackNotRemoved", err)
	}
	// The update is confirmed regardless: the row is what matters.
	if repo.state.Status != domain.UpdateConfirmed {
		t.Errorf("status = %q, want confirmed", repo.state.Status)
	}
}
