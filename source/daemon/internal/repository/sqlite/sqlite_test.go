package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/pressly/goose/v3"

	"github.com/tumika/tumika/source/daemon/internal/domain"
	"github.com/tumika/tumika/source/daemon/internal/repository/migrations"
)

// newStore opens a migrated database on a temp file. Not in-memory: the DSN
// pragmas, WAL and the dual handles are part of what is under test, and an
// in-memory database behaves differently for all three.
func newStore(t *testing.T) *Store {
	t.Helper()

	s, err := Open(t.Context(), filepath.Join(t.TempDir(), "tumika.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	if err := Migrate(t.Context(), s); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return s
}

// newStoreAtVersion opens a database migrated only as far as one version, which
// is the database a binary meets on the boot path: ConfirmBoot runs before
// Migrate, so whatever the previous binary left is what it reads and writes.
func newStoreAtVersion(t *testing.T, version int64) *Store {
	t.Helper()

	s, err := Open(t.Context(), filepath.Join(t.TempDir(), "tumika.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	goose.SetBaseFS(migrations.FS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatalf("SetDialect: %v", err)
	}
	if err := goose.UpToContext(t.Context(), s.rw, ".", version); err != nil {
		t.Fatalf("migrate up to %d: %v", version, err)
	}
	return s
}

func seedProvider(t *testing.T, s *Store, id string) {
	t.Helper()
	repo := NewProviderRepo(s)
	err := repo.Upsert(t.Context(), domain.Provider{
		ID:          id,
		DisplayName: id,
		Kind:        domain.ProviderKindCLI,
		Enabled:     true,
	})
	if err != nil {
		t.Fatalf("seed provider %s: %v", id, err)
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	s := newStore(t)

	// The daemon migrates on every boot, so running again must be a no-op.
	if err := Migrate(t.Context(), s); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}

	v, err := SchemaVersion(t.Context(), s)
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	want, err := migrations.MaxVersion()
	if err != nil {
		t.Fatalf("MaxVersion: %v", err)
	}
	if v != want {
		t.Errorf("schema version = %d, want %d", v, want)
	}
}

// Every migration has a working Down, and the daemon meets it on a database
// that already holds rows: a rollback after an update puts an older binary in
// front of a schema it has never seen, and the operator's way out is to migrate
// back. A Down that drops data, or one that cannot be re-applied, is only
// discovered at that moment.
func TestMigrationsRollBackAndForwardAgain(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	repo := NewUpdateStateRepo(s)

	watermark := time.Date(2026, 9, 20, 7, 28, 0, 0, time.UTC)
	err := repo.Put(ctx, domain.UpdateState{
		Status:      domain.UpdateConfirmed,
		FromVersion: "0.0.1",
		ToVersion:   "0.0.2",
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := repo.PutWatermark(ctx, &watermark); err != nil {
		t.Fatalf("PutWatermark: %v", err)
	}

	goose.SetBaseFS(migrations.FS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatalf("SetDialect: %v", err)
	}
	if err := goose.DownContext(ctx, s.rw, "."); err != nil {
		t.Fatalf("Down: %v", err)
	}

	// The row an older binary finds: still there, still readable, without the
	// column the newer schema added.
	var status, toVersion string
	err = s.rw.QueryRowContext(ctx,
		`SELECT status, to_version FROM update_state WHERE id = 1`).Scan(&status, &toVersion)
	if err != nil {
		t.Fatalf("read the row after Down: %v", err)
	}
	if status != string(domain.UpdateConfirmed) || toVersion != "0.0.2" {
		t.Errorf("row after Down = %q/%q, want confirmed/0.0.2", status, toVersion)
	}

	if err := Migrate(ctx, s); err != nil {
		t.Fatalf("Migrate after Down: %v", err)
	}
	got, err := repo.Get(ctx)
	if err != nil {
		t.Fatalf("Get after re-migrating: %v", err)
	}
	if got.Status != domain.UpdateConfirmed || got.ToVersion != "0.0.2" {
		t.Errorf("state = %+v, want the row preserved across the round trip", got)
	}
	// A column re-added is empty, which is exactly how a database carrying no
	// watermark reads: none at all, never a zero time.
	mark, err := repo.Watermark(ctx)
	if err != nil {
		t.Fatalf("Watermark after re-migrating: %v", err)
	}
	if mark != nil {
		t.Errorf("watermark = %v, want nil on a database that holds none", mark)
	}
}

// ConfirmBoot runs before Migrate, so the state machine's statements meet a
// database still on the schema the previous binary left. Every one of them has
// to work there, up to and including the rollback the threshold triggers —
// a statement naming a later schema's column fails, the boot goes uncounted,
// and a binary that cannot start is never rolled back.
func TestTheUpdateStateMachineWorksOnThePreviousSchema(t *testing.T) {
	s := newStoreAtVersion(t, 1)
	repo := NewUpdateStateRepo(s)
	ctx := t.Context()

	got, err := repo.Get(ctx)
	if err != nil {
		t.Fatalf("Get on schema 1: %v", err)
	}
	if got.Status != domain.UpdateIdle {
		t.Errorf("seeded status = %q, want idle", got.Status)
	}

	started := time.Now().UTC()
	err = repo.Put(ctx, domain.UpdateState{
		Status:      domain.UpdatePending,
		FromVersion: "0.0.1",
		ToVersion:   "0.0.2",
		StartedAt:   &started,
	})
	if err != nil {
		t.Fatalf("Put on schema 1: %v", err)
	}

	for i := 1; i <= domain.MaxBootAttempts; i++ {
		got, err = repo.IncrementBootAttempts(ctx)
		if err != nil {
			t.Fatalf("IncrementBootAttempts %d on schema 1: %v", i, err)
		}
		if got.BootAttempts != i {
			t.Fatalf("BootAttempts = %d, want %d", got.BootAttempts, i)
		}
	}
	if !got.ShouldRollBack() {
		t.Fatalf("after %d attempts ShouldRollBack must be true", domain.MaxBootAttempts)
	}

	// The rollback itself, which is the write that closes the boot path.
	got.Status = domain.UpdateRolledBack
	if err := repo.Put(ctx, got); err != nil {
		t.Fatalf("record the rollback on schema 1: %v", err)
	}
}

// The watermark needs the migrated schema, which is why it is read and written
// apart from the state machine and only by a daemon that is already serving.
func TestTheWatermarkRoundTrips(t *testing.T) {
	s := newStore(t)
	repo := NewUpdateStateRepo(s)
	ctx := t.Context()

	// The seeded row carries none, and reads as none rather than as a zero time
	// that would pass for a real publication time.
	got, err := repo.Watermark(ctx)
	if err != nil {
		t.Fatalf("Watermark on a fresh database: %v", err)
	}
	if got != nil {
		t.Errorf("seeded watermark = %v, want nil", got)
	}

	published := time.Now().UTC().Add(-time.Hour)
	if err := repo.PutWatermark(ctx, &published); err != nil {
		t.Fatalf("PutWatermark: %v", err)
	}
	got, err = repo.Watermark(ctx)
	if err != nil {
		t.Fatalf("Watermark: %v", err)
	}
	if got == nil || !got.Equal(published) {
		t.Errorf("watermark = %v, want %v", got, published)
	}

	// It survives the boots counted against the update, because the state
	// machine's statements do not name its column at all.
	if _, err := repo.IncrementBootAttempts(ctx); err != nil {
		t.Fatalf("IncrementBootAttempts: %v", err)
	}
	got, err = repo.Watermark(ctx)
	if err != nil {
		t.Fatalf("Watermark after an increment: %v", err)
	}
	if got == nil || !got.Equal(published) {
		t.Errorf("watermark = %v after an increment, want %v", got, published)
	}
}

// The guard that makes a post-update rollback survivable: an older binary must
// refuse a database a newer one has already migrated, rather than writing rows
// the newer schema's constraints were meant to prevent (ADR-0003).
func TestMigrateRefusesADatabaseFromANewerBinary(t *testing.T) {
	s := newStore(t)

	_, err := s.rw.ExecContext(t.Context(),
		`INSERT INTO goose_db_version (version_id, is_applied, tstamp) VALUES (9999, 1, CURRENT_TIMESTAMP)`)
	if err != nil {
		t.Fatalf("fake a newer schema: %v", err)
	}

	err = Migrate(t.Context(), s)
	if !errors.Is(err, domain.ErrSchemaTooNew) {
		t.Fatalf("Migrate error = %v, want domain.ErrSchemaTooNew", err)
	}
}

// SQLite disables foreign keys per connection by default, so the ON DELETE
// CASCADE in the schema is decoration unless the DSN turns them on. Easy to get
// wrong, and silent when it is.
func TestForeignKeysAreEnforced(t *testing.T) {
	s := newStore(t)

	_, err := NewCredentialRepo(s).Insert(t.Context(), domain.SealedCredential{
		ProviderID: "no-such-provider",
		Kind:       domain.CredentialAPIKey,
		Ciphertext: []byte("x"),
		Nonce:      []byte("n"),
		KeyRef:     "file:test",
		Meta:       domain.CredentialMeta{Status: string(domain.CredentialUnverified)},
	})
	if err == nil {
		t.Fatal("inserting a credential for an unknown provider must fail")
	}
}

func TestConfigRepoRoundTrip(t *testing.T) {
	s := newStore(t)
	repo := NewConfigRepo(s)
	ctx := t.Context()

	if _, err := repo.Get(ctx, "absent"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Get(absent) = %v, want domain.ErrNotFound", err)
	}

	if err := repo.Upsert(ctx, domain.Setting{Key: "listen", Value: []byte(`"127.0.0.1:8737"`)}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	got, err := repo.Get(ctx, "listen")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got.Value) != `"127.0.0.1:8737"` {
		t.Errorf("Value = %s", got.Value)
	}
	if got.UpdatedAt.IsZero() {
		t.Error("UpdatedAt was not stamped")
	}

	// Upsert replaces rather than duplicating.
	if err := repo.Upsert(ctx, domain.Setting{Key: "listen", Value: []byte(`"0.0.0.0:8737"`)}); err != nil {
		t.Fatalf("Upsert again: %v", err)
	}
	all, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("List returned %d settings, want 1", len(all))
	}

	if err := repo.Delete(ctx, "listen"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// Deleting an absent key is not an error — the caller wanted it gone.
	if err := repo.Delete(ctx, "listen"); err != nil {
		t.Errorf("Delete of an absent key: %v", err)
	}

	empty, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if empty == nil {
		t.Error("List must return an empty slice, not nil")
	}
}

// The registry re-seeds every provider at boot. If that overwrote `enabled`, a
// restart would silently undo the operator's decision to disable one.
func TestProviderUpsertDoesNotOverwriteEnabled(t *testing.T) {
	s := newStore(t)
	repo := NewProviderRepo(s)
	ctx := t.Context()

	seedProvider(t, s, "claude-code")
	if err := repo.SetEnabled(ctx, "claude-code", false); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}

	// Simulate the next boot: the registry seeds again, with enabled=true.
	if err := repo.Upsert(ctx, domain.Provider{
		ID:          "claude-code",
		DisplayName: "Claude Code (renamed)",
		Kind:        domain.ProviderKindCLI,
		Enabled:     true,
	}); err != nil {
		t.Fatalf("re-seed: %v", err)
	}

	got, err := repo.Get(ctx, "claude-code")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Enabled {
		t.Error("re-seeding re-enabled a provider the operator disabled")
	}
	if got.DisplayName != "Claude Code (renamed)" {
		t.Errorf("DisplayName = %q, want the driver's value to be refreshed", got.DisplayName)
	}
}

// The partial unique index is the invariant that a provider has at most one
// live credential per kind. Enforcing it in the database rather than in a
// service check is what makes it hold under concurrency.
func TestOnlyOneLiveCredentialPerProviderAndKind(t *testing.T) {
	s := newStore(t)
	repo := NewCredentialRepo(s)
	ctx := t.Context()
	seedProvider(t, s, "anthropic-api")

	cred := func(hint string) domain.SealedCredential {
		return domain.SealedCredential{
			ProviderID: "anthropic-api",
			Kind:       domain.CredentialAPIKey,
			Ciphertext: []byte("ciphertext"),
			Nonce:      []byte("nonce"),
			KeyRef:     "file:test",
			Meta:       domain.CredentialMeta{Hint: hint, Status: string(domain.CredentialActive)},
		}
	}

	if _, err := repo.Insert(ctx, cred("first")); err != nil {
		t.Fatalf("first Insert: %v", err)
	}

	_, err := repo.Insert(ctx, cred("second"))
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("second Insert = %v, want domain.ErrConflict", err)
	}

	// Retiring the incumbent frees the slot; the row itself survives, because
	// what was tried is part of the record.
	if err := repo.Retire(ctx, "anthropic-api", domain.CredentialAPIKey, domain.CredentialRevoked); err != nil {
		t.Fatalf("Retire: %v", err)
	}
	if _, err := repo.Insert(ctx, cred("second")); err != nil {
		t.Fatalf("Insert after Retire: %v", err)
	}

	live, err := repo.GetLive(ctx, "anthropic-api", domain.CredentialAPIKey)
	if err != nil {
		t.Fatalf("GetLive: %v", err)
	}
	if live.Meta.Hint != "second" {
		t.Errorf("live credential hint = %q, want the replacement", live.Meta.Hint)
	}

	// A different kind is a different slot.
	if _, err := repo.Insert(ctx, domain.SealedCredential{
		ProviderID: "anthropic-api",
		Kind:       domain.CredentialOAuthToken,
		Ciphertext: []byte("c"),
		Nonce:      []byte("n"),
		KeyRef:     "file:test",
		Meta:       domain.CredentialMeta{Status: string(domain.CredentialActive)},
	}); err != nil {
		t.Fatalf("Insert of a different kind: %v", err)
	}
}

func TestCredentialMetaRoundTrip(t *testing.T) {
	s := newStore(t)
	repo := NewCredentialRepo(s)
	ctx := t.Context()
	seedProvider(t, s, "claude-code")

	id, err := repo.Insert(ctx, domain.SealedCredential{
		ProviderID: "claude-code",
		Kind:       domain.CredentialOAuthToken,
		Ciphertext: []byte{0x01, 0x02, 0x03},
		Nonce:      []byte{0x04, 0x05},
		KeyRef:     "keychain:tumika",
		Meta:       domain.CredentialMeta{Status: string(domain.CredentialUnverified)},
	})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}

	// Timestamps are optional: nil means "not known yet", and must come back nil
	// rather than as a zero time that would satisfy a "before now" comparison.
	before, err := repo.GetLive(ctx, "claude-code", domain.CredentialOAuthToken)
	if err != nil {
		t.Fatalf("GetLive: %v", err)
	}
	if before.Meta.ExpiresAt != nil || before.Meta.IssuedAt != nil || before.Meta.LastVerifiedAt != nil {
		t.Error("absent timestamps must read back as nil")
	}

	issued := time.Now().Add(-time.Hour).UTC().Truncate(time.Nanosecond)
	expires := issued.Add(365 * 24 * time.Hour)
	verified := time.Now().UTC().Truncate(time.Nanosecond)

	_, err = repo.UpdateMeta(ctx, id, domain.CredentialMeta{
		Hint:             "…t9U",
		AccountEmail:     "someone@example.com",
		IssuedAt:         &issued,
		ExpiresAt:        &expires,
		ExpiryIsEstimate: true,
		LastVerifiedAt:   &verified,
	})
	if err != nil {
		t.Fatalf("UpdateMeta: %v", err)
	}

	got, err := repo.GetLive(ctx, "claude-code", domain.CredentialOAuthToken)
	if err != nil {
		t.Fatalf("GetLive: %v", err)
	}
	if got.Meta.IssuedAt == nil || !got.Meta.IssuedAt.Equal(issued) {
		t.Errorf("IssuedAt = %v, want %v", got.Meta.IssuedAt, issued)
	}
	if got.Meta.ExpiresAt == nil || !got.Meta.ExpiresAt.Equal(expires) {
		t.Errorf("ExpiresAt = %v, want %v", got.Meta.ExpiresAt, expires)
	}
	if !got.Meta.ExpiryIsEstimate {
		t.Error("ExpiryIsEstimate lost — a guessed expiry must not read back as a known one")
	}
	if got.Meta.AccountEmail != "someone@example.com" {
		t.Errorf("AccountEmail = %q", got.Meta.AccountEmail)
	}
	// Binary columns must survive unchanged; they are ciphertext.
	if string(got.Ciphertext) != string([]byte{0x01, 0x02, 0x03}) {
		t.Errorf("Ciphertext = %v", got.Ciphertext)
	}

	applied, err := repo.UpdateStatus(ctx, id, domain.CredentialInvalid, "api_error_status=401")
	if err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}
	if !applied {
		t.Error("the update should have applied to a live credential")
	}
	if _, err := repo.GetLive(ctx, "claude-code", domain.CredentialOAuthToken); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("an invalid credential must no longer be live, got %v", err)
	}
}

func TestOneLoginSessionInFlightPerProvider(t *testing.T) {
	s := newStore(t)
	repo := NewLoginSessionRepo(s)
	ctx := t.Context()
	seedProvider(t, s, "claude-code")

	session := func(id string, state domain.LoginState) domain.LoginSession {
		return domain.LoginSession{
			ID:         id,
			ProviderID: "claude-code",
			State:      state,
			ExpiresAt:  time.Now().Add(10 * time.Minute),
		}
	}

	if err := repo.Create(ctx, session("a", domain.LoginAwaitingBrowser)); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := repo.Create(ctx, session("b", domain.LoginPending)); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("second in-flight session = %v, want domain.ErrConflict", err)
	}

	// Startup clears the slot: a session cannot outlive the PTY that backs it.
	n, err := repo.FailAllNonTerminal(ctx, "daemon restarted")
	if err != nil {
		t.Fatalf("FailAllNonTerminal: %v", err)
	}
	if n != 1 {
		t.Errorf("FailAllNonTerminal reported %d rows, want 1", n)
	}

	got, err := repo.Get(ctx, "a")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != domain.LoginFailed || got.ErrorCode != "daemon_restarted" {
		t.Errorf("session a = %s/%s, want failed/daemon_restarted", got.State, got.ErrorCode)
	}
	if err := repo.Create(ctx, session("b", domain.LoginPending)); err != nil {
		t.Fatalf("Create after clearing: %v", err)
	}
}

func TestLoginSessionOptionalFields(t *testing.T) {
	s := newStore(t)
	repo := NewLoginSessionRepo(s)
	ctx := t.Context()
	seedProvider(t, s, "claude-code")

	if err := repo.Create(ctx, domain.LoginSession{
		ID:         "s1",
		ProviderID: "claude-code",
		State:      domain.LoginPending,
		ExpiresAt:  time.Now().Add(time.Minute),
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := repo.Get(ctx, "s1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.CredentialID != nil || got.ChildPID != nil {
		t.Error("unset optional fields must read back as nil")
	}

	pid := 4242
	got.State = domain.LoginAwaitingCode
	got.ChildPID = &pid
	got.AuthURL = "https://claude.com/cai/oauth/authorize?code=true"
	got.Prompt = "Paste code here if prompted >"
	if err := repo.Update(ctx, got); err != nil {
		t.Fatalf("Update: %v", err)
	}

	again, err := repo.Get(ctx, "s1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if again.ChildPID == nil || *again.ChildPID != pid {
		t.Errorf("ChildPID = %v, want %d", again.ChildPID, pid)
	}
	if again.State != domain.LoginAwaitingCode || again.AuthURL == "" || again.Prompt == "" {
		t.Errorf("update lost fields: %+v", again)
	}
}

func TestUpdateStateSeededAndIncremented(t *testing.T) {
	s := newStore(t)
	repo := NewUpdateStateRepo(s)
	ctx := t.Context()

	// The initial migration seeds the single row, so Get never fails on a fresh
	// database — the update path must not have to special-case first boot.
	got, err := repo.Get(ctx)
	if err != nil {
		t.Fatalf("Get on a fresh database: %v", err)
	}
	if got.Status != domain.UpdateIdle {
		t.Errorf("seeded status = %q, want idle", got.Status)
	}

	started := time.Now().UTC().Truncate(time.Nanosecond)
	err = repo.Put(ctx, domain.UpdateState{
		Status:      domain.UpdatePending,
		FromVersion: "v0.0.1",
		ToVersion:   "v0.0.2",
		StartedAt:   &started,
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	for i := 1; i <= domain.MaxBootAttempts; i++ {
		got, err = repo.IncrementBootAttempts(ctx)
		if err != nil {
			t.Fatalf("IncrementBootAttempts: %v", err)
		}
		if got.BootAttempts != i {
			t.Fatalf("BootAttempts = %d, want %d", got.BootAttempts, i)
		}
	}
	if !got.ShouldRollBack() {
		t.Errorf("after %d attempts ShouldRollBack must be true", domain.MaxBootAttempts)
	}
	if got.StartedAt == nil || !got.StartedAt.Equal(started) {
		t.Errorf("StartedAt = %v, want %v", got.StartedAt, started)
	}
}

func TestInTxRollsBackOnError(t *testing.T) {
	s := newStore(t)
	repo := NewConfigRepo(s)
	ctx := t.Context()

	sentinel := errors.New("business rule said no")
	err := s.InTx(ctx, func(ctx context.Context) error {
		if err := repo.Upsert(ctx, domain.Setting{Key: "k", Value: []byte(`1`)}); err != nil {
			return err
		}
		// Read-your-own-writes inside the transaction: this only works because
		// reads inside a transaction go through the transaction rather than the
		// separate read handle.
		if _, err := repo.Get(ctx, "k"); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("InTx error = %v, want the callback's error", err)
	}

	if _, err := repo.Get(ctx, "k"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("write survived a rolled-back transaction: %v", err)
	}
}

func TestInTxCommits(t *testing.T) {
	s := newStore(t)
	repo := NewConfigRepo(s)
	ctx := t.Context()

	err := s.InTx(ctx, func(ctx context.Context) error {
		// A nested InTx joins the outer transaction; opening a second one would
		// deadlock against the single writer.
		return s.InTx(ctx, func(ctx context.Context) error {
			return repo.Upsert(ctx, domain.Setting{Key: "k", Value: []byte(`1`)})
		})
	})
	if err != nil {
		t.Fatalf("InTx: %v", err)
	}

	if _, err := repo.Get(ctx, "k"); err != nil {
		t.Fatalf("committed write not visible: %v", err)
	}
}
