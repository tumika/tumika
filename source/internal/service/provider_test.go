package service_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tumika/tumika/source/internal/domain"
	"github.com/tumika/tumika/source/internal/platform/provider"
	"github.com/tumika/tumika/source/internal/platform/secrets"
	"github.com/tumika/tumika/source/internal/service"
)

// These run against in-memory fakes: no database, no HTTP, no daemon. That is
// the payoff the layering was built for, and the credential rules below are
// exactly the kind that would otherwise need an integration test to reach.

const (
	fakeProviderID = "fake-provider"
	goodSecret     = "sk-ant-good-000000000000"
	badSecret      = "sk-ant-bad-0000000000000"
)

// fakeDriver is a provider whose verification verdict the test controls.
type fakeDriver struct {
	// verdict maps a secret to the status Verify reports.
	verdict map[string]domain.CredentialStatus
	// err, when set, means the check could not be carried out at all.
	err   error
	calls int
	meta  domain.CredentialMeta
}

func (f *fakeDriver) Descriptor() domain.Descriptor {
	return domain.Descriptor{
		ID:          fakeProviderID,
		DisplayName: "Fake",
		Kind:        domain.ProviderKindHTTP,
		AuthMethods: []domain.AuthMethod{domain.AuthAPIKey},
	}
}

func (f *fakeDriver) Preflight(context.Context) (domain.Preflight, error) {
	return domain.Preflight{Ready: true}, nil
}

func (f *fakeDriver) AcceptedMethods() []domain.AuthMethod {
	return []domain.AuthMethod{domain.AuthAPIKey}
}

func (f *fakeDriver) ValidateSecret(_ domain.AuthMethod, secret string) error {
	if !strings.HasPrefix(secret, "sk-ant-") {
		return domain.ErrCredentialInvalid
	}
	return nil
}

func (f *fakeDriver) Materialize(_ domain.AuthMethod, secret string) (domain.Credential, error) {
	return domain.Credential{
		ProviderID: fakeProviderID,
		Kind:       domain.CredentialAPIKey,
		Secret:     secret,
		Meta:       domain.CredentialMeta{Hint: "…" + secret[len(secret)-4:]},
	}, nil
}

func (f *fakeDriver) Verify(_ context.Context, c domain.Credential) (domain.CredentialMeta, error) {
	f.calls++
	if f.err != nil {
		return domain.CredentialMeta{}, f.err
	}

	status := f.verdict[c.Secret]
	if status == "" {
		status = domain.CredentialActive
	}

	meta := f.meta
	meta.Status = string(status)
	now := time.Now().UTC()
	meta.LastVerifiedAt = &now
	if status == domain.CredentialInvalid {
		meta.LastVerifyError = "rejected by the provider"
	}
	return meta, nil
}

// fakeCredRepo is an in-memory CredentialRepository with the liveness rules the
// real one enforces through a partial unique index.
type fakeCredRepo struct {
	rows   map[int64]*domain.SealedCredential
	nextID int64
}

func newCredRepo() *fakeCredRepo {
	return &fakeCredRepo{rows: map[int64]*domain.SealedCredential{}, nextID: 1}
}

func live(status string) bool {
	return domain.CredentialStatus(status).Live()
}

func (f *fakeCredRepo) GetLive(_ context.Context, providerID, kind string) (domain.SealedCredential, error) {
	for _, row := range f.rows {
		if row.ProviderID == providerID && row.Kind == kind && live(row.Meta.Status) {
			return *row, nil
		}
	}
	return domain.SealedCredential{}, domain.ErrNotFound
}

func (f *fakeCredRepo) ListLive(context.Context) ([]domain.SealedCredential, error) {
	var out []domain.SealedCredential
	for _, row := range f.rows {
		if live(row.Meta.Status) {
			out = append(out, *row)
		}
	}
	return out, nil
}

func (f *fakeCredRepo) Insert(_ context.Context, c domain.SealedCredential) (int64, error) {
	for _, row := range f.rows {
		if row.ProviderID == c.ProviderID && row.Kind == c.Kind && live(row.Meta.Status) {
			return 0, domain.ErrConflict // the partial unique index
		}
	}
	id := f.nextID
	f.nextID++
	c.ID = id
	f.rows[id] = &c
	return id, nil
}

func (f *fakeCredRepo) UpdateStatus(_ context.Context, id int64, status domain.CredentialStatus, verifyErr string) (bool, error) {
	row, ok := f.rows[id]
	if !ok || !live(row.Meta.Status) {
		return false, nil
	}
	row.Meta.Status = string(status)
	row.Meta.LastVerifyError = verifyErr
	return true, nil
}

func (f *fakeCredRepo) UpdateMeta(_ context.Context, id int64, meta domain.CredentialMeta) (bool, error) {
	row, ok := f.rows[id]
	if !ok || !live(row.Meta.Status) {
		return false, nil
	}
	status := row.Meta.Status
	row.Meta = meta
	row.Meta.Status = status
	return true, nil
}

func (f *fakeCredRepo) Retire(_ context.Context, providerID, kind string, status domain.CredentialStatus) error {
	for _, row := range f.rows {
		if row.ProviderID == providerID && row.Kind == kind && live(row.Meta.Status) {
			row.Meta.Status = string(status)
		}
	}
	return nil
}

func (f *fakeCredRepo) Delete(_ context.Context, id int64) error {
	delete(f.rows, id)
	return nil
}

type fakeProviderRepo struct{ rows map[string]domain.Provider }

func (f *fakeProviderRepo) Get(_ context.Context, id string) (domain.Provider, error) {
	p, ok := f.rows[id]
	if !ok {
		return domain.Provider{}, domain.ErrNotFound
	}
	return p, nil
}

func (f *fakeProviderRepo) List(context.Context) ([]domain.Provider, error) {
	out := make([]domain.Provider, 0, len(f.rows))
	for _, p := range f.rows {
		out = append(out, p)
	}
	return out, nil
}

func (f *fakeProviderRepo) Upsert(_ context.Context, p domain.Provider) error {
	if existing, ok := f.rows[p.ID]; ok {
		p.Enabled = existing.Enabled // the real upsert never overwrites this
	}
	f.rows[p.ID] = p
	return nil
}

func (f *fakeProviderRepo) SetEnabled(_ context.Context, id string, enabled bool) error {
	p := f.rows[id]
	p.Enabled = enabled
	f.rows[id] = p
	return nil
}

func newProviderService(t *testing.T, driver *fakeDriver) (service.ProviderService, *fakeCredRepo) {
	t.Helper()

	registry, err := provider.NewRegistry(driver)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	store, err := secrets.NewFileKeyStore(t.TempDir() + "/master.key")
	if err != nil {
		t.Fatalf("NewFileKeyStore: %v", err)
	}
	sealer, err := secrets.New(store)
	if err != nil {
		t.Fatalf("New sealer: %v", err)
	}

	cfg, _, _ := newService(t)
	creds := newCredRepo()
	repo := &fakeProviderRepo{rows: map[string]domain.Provider{}}

	svc := service.NewProviderService(registry, repo, creds, sealer, cfg, &fakeTxer{})
	if err := svc.Seed(t.Context()); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	return svc, creds
}

func TestSubmitStoresAndVerifies(t *testing.T) {
	driver := &fakeDriver{verdict: map[string]domain.CredentialStatus{goodSecret: domain.CredentialActive}}
	svc, creds := newProviderService(t, driver)

	meta, err := svc.SubmitSecret(t.Context(), fakeProviderID, domain.AuthAPIKey, goodSecret)
	if err != nil {
		t.Fatalf("SubmitSecret: %v", err)
	}
	if meta.Status != string(domain.CredentialActive) {
		t.Errorf("Status = %q", meta.Status)
	}

	stored, err := creds.GetLive(t.Context(), fakeProviderID, domain.CredentialAPIKey)
	if err != nil {
		t.Fatalf("GetLive: %v", err)
	}
	if string(stored.Ciphertext) == goodSecret {
		t.Error("the secret was stored in plaintext")
	}
}

// Replacing a credential must not revoke the working one until the replacement
// is proven. Otherwise a single mistyped key leaves the provider with nothing
// live and no way back.
func TestARejectedReplacementLeavesTheIncumbentInPlace(t *testing.T) {
	driver := &fakeDriver{verdict: map[string]domain.CredentialStatus{
		goodSecret: domain.CredentialActive,
		badSecret:  domain.CredentialInvalid,
	}}
	svc, creds := newProviderService(t, driver)
	ctx := t.Context()

	if _, err := svc.SubmitSecret(ctx, fakeProviderID, domain.AuthAPIKey, goodSecret); err != nil {
		t.Fatalf("first submit: %v", err)
	}
	first, err := creds.GetLive(ctx, fakeProviderID, domain.CredentialAPIKey)
	if err != nil {
		t.Fatalf("GetLive: %v", err)
	}

	_, err = svc.SubmitSecret(ctx, fakeProviderID, domain.AuthAPIKey, badSecret)
	if !errors.Is(err, domain.ErrCredentialInvalid) {
		t.Fatalf("submitting a rejected key = %v, want ErrCredentialInvalid", err)
	}
	if !strings.Contains(err.Error(), "left in place") {
		t.Errorf("the error should say the incumbent survived, got: %v", err)
	}

	after, err := creds.GetLive(ctx, fakeProviderID, domain.CredentialAPIKey)
	if err != nil {
		t.Fatalf("the working credential was revoked by a failed replacement: %v", err)
	}
	if after.ID != first.ID {
		t.Error("the incumbent was replaced by a credential that failed verification")
	}
	if after.Meta.Status != string(domain.CredentialActive) {
		t.Errorf("the incumbent's status changed to %q", after.Meta.Status)
	}
}

// The same applies when the provider cannot be reached at all: an outage must
// not cost the operator a working credential.
func TestAnUnreachableProviderLeavesTheIncumbentInPlace(t *testing.T) {
	driver := &fakeDriver{}
	svc, creds := newProviderService(t, driver)
	ctx := t.Context()

	if _, err := svc.SubmitSecret(ctx, fakeProviderID, domain.AuthAPIKey, goodSecret); err != nil {
		t.Fatalf("first submit: %v", err)
	}

	driver.err = errors.New("connection refused")
	_, err := svc.SubmitSecret(ctx, fakeProviderID, domain.AuthAPIKey, badSecret)
	if !errors.Is(err, domain.ErrProviderUnavailable) {
		t.Fatalf("= %v, want ErrProviderUnavailable", err)
	}

	if _, err := creds.GetLive(ctx, fakeProviderID, domain.CredentialAPIKey); err != nil {
		t.Errorf("the working credential was lost to a provider outage: %v", err)
	}
}

// A FIRST credential is kept even when it cannot be verified — there is nothing
// to lose, and an outage should not stop an operator supplying one.
func TestAFirstCredentialSurvivesAnUnverifiableProvider(t *testing.T) {
	driver := &fakeDriver{err: errors.New("connection refused")}
	svc, creds := newProviderService(t, driver)

	_, err := svc.SubmitSecret(t.Context(), fakeProviderID, domain.AuthAPIKey, goodSecret)
	if !errors.Is(err, domain.ErrProviderUnavailable) {
		t.Fatalf("= %v, want ErrProviderUnavailable", err)
	}

	stored, err := creds.GetLive(t.Context(), fakeProviderID, domain.CredentialAPIKey)
	if err != nil {
		t.Fatalf("the credential should have been kept for the monitor to re-check: %v", err)
	}
	if stored.Meta.Status != string(domain.CredentialUnverified) {
		t.Errorf("Status = %q, want unverified", stored.Meta.Status)
	}
}

// A driver reports only what it learned. Replacing stored metadata wholesale
// would null an account and expiry established by an interactive login, so
// expiry warnings would stop working after the first re-verification.
func TestVerificationMergesMetadataRatherThanReplacingIt(t *testing.T) {
	driver := &fakeDriver{}
	svc, creds := newProviderService(t, driver)
	ctx := t.Context()

	issued := time.Now().Add(-time.Hour).UTC()
	expires := issued.Add(365 * 24 * time.Hour)

	_, err := svc.StoreCredential(ctx, domain.Credential{
		ProviderID: fakeProviderID,
		Kind:       domain.CredentialAPIKey,
		Secret:     goodSecret,
		Meta: domain.CredentialMeta{
			Hint:             "…cccc",
			AccountEmail:     "someone@example.com",
			IssuedAt:         &issued,
			ExpiresAt:        &expires,
			ExpiryIsEstimate: true,
		},
	})
	if err != nil {
		t.Fatalf("StoreCredential: %v", err)
	}

	// The driver reports only a status and a timestamp.
	if _, err := svc.VerifyCredential(ctx, fakeProviderID); err != nil {
		t.Fatalf("VerifyCredential: %v", err)
	}

	stored, err := creds.GetLive(ctx, fakeProviderID, domain.CredentialAPIKey)
	if err != nil {
		t.Fatalf("GetLive: %v", err)
	}
	if stored.Meta.AccountEmail != "someone@example.com" {
		t.Errorf("AccountEmail was lost: %q", stored.Meta.AccountEmail)
	}
	if stored.Meta.ExpiresAt == nil {
		t.Error("ExpiresAt was nulled; expiry warnings would stop working")
	}
	if !stored.Meta.ExpiryIsEstimate {
		t.Error("ExpiryIsEstimate was lost")
	}
	if stored.Meta.LastVerifiedAt == nil {
		t.Error("LastVerifiedAt was not updated")
	}
}

// Verification deliberately runs outside a transaction, so a verdict can arrive
// after the row it describes has been retired. Writing it anyway would bring a
// deleted credential back to life.
func TestALateVerdictDoesNotResurrectADeletedCredential(t *testing.T) {
	driver := &fakeDriver{}
	svc, creds := newProviderService(t, driver)
	ctx := t.Context()

	if _, err := svc.SubmitSecret(ctx, fakeProviderID, domain.AuthAPIKey, goodSecret); err != nil {
		t.Fatalf("submit: %v", err)
	}
	row, err := creds.GetLive(ctx, fakeProviderID, domain.CredentialAPIKey)
	if err != nil {
		t.Fatalf("GetLive: %v", err)
	}

	// The operator deletes the credential while a verification is in flight.
	if err := svc.DeleteCredential(ctx, fakeProviderID); err != nil {
		t.Fatalf("DeleteCredential: %v", err)
	}

	// The late verdict lands on a row that is no longer live.
	applied, err := creds.UpdateStatus(ctx, row.ID, domain.CredentialActive, "")
	if err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}
	if applied {
		t.Error("a status update was applied to a retired credential, resurrecting it")
	}

	if _, err := creds.GetLive(ctx, fakeProviderID, domain.CredentialAPIKey); !errors.Is(err, domain.ErrNotFound) {
		t.Error("the deleted credential is live again")
	}
}

// An explicit verification asks "does this still work"; "no" is an answer, and
// it comes back as metadata rather than an error.
func TestVerifyReportsARejectionAsMetadata(t *testing.T) {
	driver := &fakeDriver{}
	svc, _ := newProviderService(t, driver)
	ctx := t.Context()

	if _, err := svc.SubmitSecret(ctx, fakeProviderID, domain.AuthAPIKey, goodSecret); err != nil {
		t.Fatalf("submit: %v", err)
	}

	driver.verdict = map[string]domain.CredentialStatus{goodSecret: domain.CredentialInvalid}
	meta, err := svc.VerifyCredential(ctx, fakeProviderID)
	if err != nil {
		t.Fatalf("a rejection is a result, not an error: %v", err)
	}
	if meta.Status != string(domain.CredentialInvalid) {
		t.Errorf("Status = %q, want invalid", meta.Status)
	}
	if meta.LastVerifyError == "" {
		t.Error("the reason was not reported")
	}
}

func TestListAndSelect(t *testing.T) {
	driver := &fakeDriver{}
	svc, _ := newProviderService(t, driver)
	ctx := t.Context()

	views, err := svc.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(views) != 1 || views[0].ID != fakeProviderID {
		t.Fatalf("List = %+v", views)
	}
	if views[0].Credential != nil {
		t.Error("no credential has been stored yet")
	}
	if views[0].RequiresInteractiveAuth {
		t.Error("an API key provider must not require an interactive login")
	}

	if err := svc.Select(ctx, fakeProviderID); err != nil {
		t.Fatalf("Select: %v", err)
	}
	view, err := svc.Get(ctx, fakeProviderID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !view.Selected {
		t.Error("the provider was not marked selected")
	}

	if err := svc.Select(ctx, "nope"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Select(unknown) = %v, want ErrNotFound", err)
	}
	if _, err := svc.Get(ctx, "nope"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Get(unknown) = %v, want ErrNotFound", err)
	}
}

// Shape is checked before the provider is called at all.
func TestSubmitRejectsAMalformedSecretWithoutCallingTheProvider(t *testing.T) {
	driver := &fakeDriver{}
	svc, _ := newProviderService(t, driver)

	if _, err := svc.SubmitSecret(t.Context(), fakeProviderID, domain.AuthAPIKey, "nonsense"); err == nil {
		t.Fatal("a malformed secret was accepted")
	}
	if driver.calls != 0 {
		t.Errorf("the provider was called %d times for a secret that could not be valid", driver.calls)
	}
}

func TestPreflightAndSeedAreIdempotent(t *testing.T) {
	driver := &fakeDriver{}
	svc, _ := newProviderService(t, driver)
	ctx := t.Context()

	if err := svc.Seed(ctx); err != nil {
		t.Fatalf("second Seed: %v", err)
	}

	pf, err := svc.Preflight(ctx, fakeProviderID)
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if !pf.Ready {
		t.Error("the fake driver reports not ready")
	}
	if _, err := svc.Preflight(ctx, "nope"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Preflight(unknown) = %v, want ErrNotFound", err)
	}
}
