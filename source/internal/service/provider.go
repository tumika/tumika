package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/tumika/tumika/source/internal/domain"
	"github.com/tumika/tumika/source/internal/platform/provider"
	"github.com/tumika/tumika/source/internal/platform/secrets"
	"github.com/tumika/tumika/source/internal/repository"
)

// ProviderService owns providers and their credentials.
//
// It owns two repositories, and it is the ONLY way anything reaches a
// credential: LoginService will call StoreCredential rather than touching
// CredentialRepository, and CredentialMonitorRunner will call VerifyCredential.
// The sealing AAD and the verify-before-active rule live here, and a second
// writer would silently skip both. See
// .agents/rules/a-repository-has-exactly-one-owning-service.md.
type ProviderService interface {
	// Seed writes the registry's providers into the database. Run at boot, and
	// idempotent.
	Seed(ctx context.Context) error

	List(ctx context.Context) ([]domain.ProviderView, error)
	Get(ctx context.Context, id string) (domain.ProviderView, error)
	Preflight(ctx context.Context, id string) (domain.Preflight, error)
	// Select records which provider inference should use.
	Select(ctx context.Context, id string) error

	// SubmitSecret is the non-interactive path: the caller already holds the
	// secret. Validate, seal, store, verify.
	SubmitSecret(ctx context.Context, id string, method domain.AuthMethod, secret string) (domain.CredentialMeta, error)
	// StoreCredential seals and stores a credential obtained some other way —
	// an interactive login, at the PTY step. Same rules, same path.
	StoreCredential(ctx context.Context, c domain.Credential) (domain.CredentialMeta, error)
	// VerifyCredential re-checks the stored credential against its provider.
	VerifyCredential(ctx context.Context, id string) (domain.CredentialMeta, error)
	DeleteCredential(ctx context.Context, id string) error
}

type providerService struct {
	registry *provider.Registry
	repo     repository.ProviderRepository
	creds    repository.CredentialRepository
	sealer   secrets.Sealer
	config   ConfigService
	tx       repository.Txer
}

// NewProviderService builds the service.
func NewProviderService(
	registry *provider.Registry,
	repo repository.ProviderRepository,
	creds repository.CredentialRepository,
	sealer secrets.Sealer,
	config ConfigService,
	tx repository.Txer,
) ProviderService {
	return &providerService{
		registry: registry,
		repo:     repo,
		creds:    creds,
		sealer:   sealer,
		config:   config,
		tx:       tx,
	}
}

// Seed writes every registered provider into the database.
//
// The registry is the source of truth for which providers EXIST; the table holds
// the mutable half an operator can change. The upsert deliberately does not
// touch `enabled`, so a restart cannot undo a decision to disable one.
func (s *providerService) Seed(ctx context.Context) error {
	return s.tx.InTx(ctx, func(ctx context.Context) error {
		for _, p := range s.registry.List() {
			d := p.Descriptor()
			err := s.repo.Upsert(ctx, domain.Provider{
				ID:          d.ID,
				DisplayName: d.DisplayName,
				Kind:        d.Kind,
				Enabled:     true, // only applied on insert
			})
			if err != nil {
				return fmt.Errorf("seed provider %s: %w", d.ID, err)
			}
		}
		return nil
	})
}

func (s *providerService) List(ctx context.Context) ([]domain.ProviderView, error) {
	selected, err := String(ctx, s.config, KeyProviderSelected)
	if err != nil {
		return nil, err
	}

	stored, err := s.repo.List(ctx)
	if err != nil {
		return nil, err
	}
	enabled := make(map[string]bool, len(stored))
	for _, p := range stored {
		enabled[p.ID] = p.Enabled
	}

	live, err := s.creds.ListLive(ctx)
	if err != nil {
		return nil, err
	}
	byProvider := make(map[string]domain.CredentialMeta, len(live))
	for _, c := range live {
		byProvider[c.ProviderID] = c.Meta
	}

	out := make([]domain.ProviderView, 0, len(s.registry.IDs()))
	for _, p := range s.registry.List() {
		d := p.Descriptor()
		view := domain.ProviderView{
			Descriptor:              d,
			RequiresInteractiveAuth: d.RequiresInteractiveAuth(),
			Enabled:                 enabled[d.ID],
			Selected:                d.ID == selected,
		}
		if meta, ok := byProvider[d.ID]; ok {
			view.Credential = &meta
		}
		out = append(out, view)
	}
	return out, nil
}

func (s *providerService) Get(ctx context.Context, id string) (domain.ProviderView, error) {
	views, err := s.List(ctx)
	if err != nil {
		return domain.ProviderView{}, err
	}
	for _, v := range views {
		if v.ID == id {
			return v, nil
		}
	}
	return domain.ProviderView{}, fmt.Errorf("%w: provider %q", domain.ErrNotFound, id)
}

func (s *providerService) Preflight(ctx context.Context, id string) (domain.Preflight, error) {
	p, err := s.registry.Get(id)
	if err != nil {
		return domain.Preflight{}, err
	}
	return p.Preflight(ctx)
}

func (s *providerService) Select(ctx context.Context, id string) error {
	if _, err := s.registry.Get(id); err != nil {
		return err
	}
	// Selection goes through ConfigService because the settings table is owned
	// there. Writing it directly would be a second writer to someone else's
	// repository.
	encoded, err := json.Marshal(id)
	if err != nil {
		return err
	}
	_, err = s.config.Set(ctx, map[string]json.RawMessage{KeyProviderSelected: encoded})
	return err
}

func (s *providerService) SubmitSecret(
	ctx context.Context, id string, method domain.AuthMethod, secret string,
) (domain.CredentialMeta, error) {
	static, err := s.registry.StaticAuthenticator(id)
	if err != nil {
		return domain.CredentialMeta{}, err
	}

	// Shape first, and no network: an obviously wrong paste should fail
	// immediately rather than after a round trip that fails confusingly.
	if err := static.ValidateSecret(method, secret); err != nil {
		return domain.CredentialMeta{}, err
	}

	cred, err := static.Materialize(method, secret)
	if err != nil {
		return domain.CredentialMeta{}, err
	}
	cred.ProviderID = id

	return s.StoreCredential(ctx, cred)
}

// StoreCredential seals, stores and then verifies.
//
// The ordering is the interesting part, and it is why the schema has an
// `unverified` status at all:
//
//  1. Retire the incumbent and insert the new credential as `unverified`, in ONE
//     transaction.
//  2. Verify over the network, holding NO transaction.
//  3. Record the verdict in a second transaction.
//
// Verifying inside the transaction would hold SQLite's single write lock across
// a network call — so a slow or hanging provider would block every other write
// in the daemon for as long as it took. Storing before verifying also means a
// credential that arrives during an outage is kept rather than discarded, and
// the monitor re-checks it later.
func (s *providerService) StoreCredential(ctx context.Context, cred domain.Credential) (domain.CredentialMeta, error) {
	return s.store(ctx, cred)
}

func (s *providerService) store(ctx context.Context, cred domain.Credential) (domain.CredentialMeta, error) {
	if cred.ProviderID == "" {
		return domain.CredentialMeta{}, fmt.Errorf("%w: no provider", domain.ErrCredentialInvalid)
	}
	if cred.Secret == "" {
		return domain.CredentialMeta{}, fmt.Errorf("%w: no secret", domain.ErrCredentialInvalid)
	}
	// Checked here rather than left to the database's CHECK constraint: the kind
	// is bound into the sealing AAD and assumed by openLive and DeleteCredential,
	// so an unknown one would seal successfully and then surface as an opaque
	// 500 from a constraint violation.
	if cred.Kind != domain.CredentialOAuthToken && cred.Kind != domain.CredentialAPIKey {
		return domain.CredentialMeta{}, fmt.Errorf("%w: unknown credential kind %q",
			domain.ErrCredentialInvalid, cred.Kind)
	}
	if _, err := s.registry.Get(cred.ProviderID); err != nil {
		return domain.CredentialMeta{}, err
	}

	sealed := domain.SealedCredential{
		ProviderID: cred.ProviderID,
		Kind:       cred.Kind,
		Meta:       cred.Meta,
	}
	sealed.Meta.Status = string(domain.CredentialUnverified)

	// The AAD binds this ciphertext to this provider and kind, so a row cannot
	// be transplanted between them.
	box, err := s.sealer.Seal([]byte(cred.Secret), sealed.AAD())
	if err != nil {
		return domain.CredentialMeta{}, fmt.Errorf("seal credential: %w", err)
	}
	sealed.Ciphertext, sealed.Nonce = box.Ciphertext, box.Nonce
	sealed.KeyRef, sealed.Cipher = box.KeyRef, box.Cipher

	// Is there an incumbent? Replacing one is a different operation from
	// establishing the first, and the difference matters.
	//
	// Asked per PROVIDER, not per kind. The partial unique index is per kind, so
	// submitting an api_key while an oauth_token was live used to find no
	// incumbent, take the first-credential path, and leave two live rows — after
	// which openLive picked one of them and List reported whichever came last.
	// A provider has one credential in use; which kind it is is the provider's
	// business, not a second slot.
	replacing := false
	for _, kind := range credentialKinds {
		if _, err := s.creds.GetLive(ctx, cred.ProviderID, kind); err == nil {
			replacing = true
			break
		} else if !errors.Is(err, domain.ErrNotFound) {
			return domain.CredentialMeta{}, err
		}
	}

	// REPLACING: prove the candidate before retiring what works.
	//
	// Storing first would mean a well-shaped but wrong key retires the working
	// credential and then fails verification, leaving the provider with nothing
	// live and no way to get the old one back. Verifying first costs the
	// "keep it during an outage" property for replacements only — and keeping a
	// working credential beats keeping an unproven one.
	if replacing {
		meta, err := s.check(ctx, cred)
		if err != nil {
			return meta, fmt.Errorf("%w: the existing credential was left in place", err)
		}

		// Anything short of ACTIVE leaves the incumbent alone.
		//
		// Testing only for `invalid` was not enough: a driver reporting
		// `expired` — or reporting nothing at all, which its contract permits —
		// would have retired a working credential and inserted a row that is not
		// live, leaving the provider with none and no way back. That is exactly
		// the outcome verifying-before-retiring exists to prevent, so the
		// condition is a positive test for the one status that justifies the
		// swap.
		if meta.Status != string(domain.CredentialActive) {
			status := meta.Status
			if status == "" {
				status = "unverified"
			}
			return meta, fmt.Errorf("%w: the replacement verified as %s (%s); "+
				"the existing credential was left in place",
				domain.ErrCredentialInvalid, status, meta.LastVerifyError)
		}
		sealed.Meta = mergeMeta(cred.Meta, meta)

		err = s.tx.InTx(ctx, func(ctx context.Context) error {
			// Every kind, so the provider is left with exactly one live
			// credential.
			for _, kind := range credentialKinds {
				if err := s.creds.Retire(ctx, cred.ProviderID, kind, domain.CredentialRevoked); err != nil {
					return err
				}
			}
			_, err := s.creds.Insert(ctx, sealed)
			return err
		})
		if err != nil {
			return domain.CredentialMeta{}, err
		}
		return sealed.Meta, nil
	}

	// FIRST credential: store it, then verify. There is nothing to lose by
	// keeping an unproven credential, and a provider outage should not stop an
	// operator supplying one — the monitor re-checks it later.
	var id int64
	err = s.tx.InTx(ctx, func(ctx context.Context) error {
		var err error
		id, err = s.creds.Insert(ctx, sealed)
		return err
	})
	if err != nil {
		return domain.CredentialMeta{}, err
	}

	meta, err := s.verify(ctx, cred.ProviderID, id, cred)
	if err != nil {
		// Stored but unverifiable: the provider could not be reached. That is a
		// real, recorded state rather than a failure to store — so what comes
		// back is the metadata that was WRITTEN, which has Status forced to
		// unverified, not the caller's, which may claim otherwise.
		return sealed.Meta, err
	}
	if meta.Status == string(domain.CredentialInvalid) {
		return meta, fmt.Errorf("%w: %s", domain.ErrCredentialInvalid, meta.LastVerifyError)
	}
	return meta, nil
}

// check runs the driver's verification without touching the database.
//
// A failure here means the check could not be CARRIED OUT, which says nothing
// about the credential — so it is wrapped in ErrProviderUnavailable and never
// treated as a verdict. Both callers depend on that distinction: one would
// otherwise revoke a working credential over an outage, the other would report
// tumika as broken when the provider is.
func (s *providerService) check(ctx context.Context, cred domain.Credential) (domain.CredentialMeta, error) {
	checker, err := s.registry.HealthChecker(cred.ProviderID)
	if err != nil {
		return domain.CredentialMeta{}, err
	}

	meta, err := checker.Verify(ctx, cred)
	if err != nil {
		return meta, fmt.Errorf("%w: %w", domain.ErrProviderUnavailable, err)
	}
	return meta, nil
}

// mergeMeta layers what a driver reported over what is already stored.
//
// A driver reports only what it learned. anthropicapi.Verify, for instance,
// returns a hint, a status and a timestamp — so replacing the stored metadata
// wholesale would null the account and expiry an interactive login had
// established, and expiry warnings would silently stop working after the first
// re-verification.
func mergeMeta(stored, fresh domain.CredentialMeta) domain.CredentialMeta {
	merged := stored

	if fresh.Status != "" {
		merged.Status = fresh.Status
	}
	if fresh.Hint != "" {
		merged.Hint = fresh.Hint
	}
	if fresh.AccountEmail != "" {
		merged.AccountEmail = fresh.AccountEmail
	}
	if fresh.IssuedAt != nil {
		merged.IssuedAt = fresh.IssuedAt
		merged.ExpiryIsEstimate = fresh.ExpiryIsEstimate
	}
	if fresh.ExpiresAt != nil {
		merged.ExpiresAt = fresh.ExpiresAt
		merged.ExpiryIsEstimate = fresh.ExpiryIsEstimate
	}
	if fresh.LastVerifiedAt != nil {
		merged.LastVerifiedAt = fresh.LastVerifiedAt
	}
	// Always taken from the fresh result, including when it clears.
	merged.LastVerifyError = fresh.LastVerifyError

	return merged
}

func (s *providerService) VerifyCredential(ctx context.Context, id string) (domain.CredentialMeta, error) {
	if _, err := s.registry.Get(id); err != nil {
		return domain.CredentialMeta{}, err
	}

	cred, sealedID, err := s.openLive(ctx, id)
	if err != nil {
		return domain.CredentialMeta{}, err
	}

	// A rejection is a RESULT here, not an error: an explicit verification asked
	// "does this still work", and "no" is an answer. The submission path turns
	// the same verdict into an error, because there the caller was trying to
	// establish a credential and did not succeed.
	return s.verify(ctx, id, sealedID, cred)
}

// verify runs the driver's check and records the verdict.
//
// A rejected credential is a RESULT and is recorded; a check that could not be
// carried out is an ERROR and changes nothing. Marking a credential invalid
// because the network was down would revoke a working key over an outage.
func (s *providerService) verify(
	ctx context.Context, providerID string, sealedID int64, cred domain.Credential,
) (domain.CredentialMeta, error) {
	fresh, verifyErr := s.check(ctx, cred)
	if verifyErr != nil {
		// The check could not be carried out. Nothing is recorded — marking a
		// credential invalid because the provider was unreachable would revoke a
		// working key over someone else's outage.
		return cred.Meta, verifyErr
	}

	merged := mergeMeta(cred.Meta, fresh)
	if merged.Status == "" {
		merged.Status = string(domain.CredentialUnverified)
	}

	var applied bool
	err := s.tx.InTx(ctx, func(ctx context.Context) error {
		var err error
		if applied, err = s.creds.UpdateMeta(ctx, sealedID, merged); err != nil {
			return err
		}
		applied, err = s.creds.UpdateStatus(ctx, sealedID, domain.CredentialStatus(merged.Status), merged.LastVerifyError)
		return err
	})
	if err != nil {
		return merged, err
	}

	// The row was retired or replaced while the provider was being called.
	// Writing the verdict anyway would resurrect a revoked credential, so it is
	// discarded — a normal outcome of verifying outside a transaction, not a
	// failure.
	if !applied {
		return merged, fmt.Errorf("%w: the credential was replaced or removed while it was being verified",
			domain.ErrSuperseded)
	}

	return merged, nil
}

// openLive unseals the live credential for a provider.
// openLive unseals the credential to verify for a provider.
//
// It prefers a live one, and falls back to the most recent credential the
// operator has not revoked. Without the fallback, a credential rejected once —
// including by a provider-side hiccup — could never be re-checked: GetLive
// excludes 'invalid', so an explicit verification would answer 404 and the
// monitor would never look at it again. Being unusable and being unrecoverable
// are different things.
func (s *providerService) openLive(ctx context.Context, providerID string) (domain.Credential, int64, error) {
	for _, lookup := range []func(context.Context, string, string) (domain.SealedCredential, error){
		s.creds.GetLive,
		s.creds.GetLatest,
	} {
		sealed, found, err := firstOf(ctx, lookup, providerID)
		if err != nil {
			return domain.Credential{}, 0, err
		}
		if !found {
			continue
		}

		plaintext, err := s.sealer.Open(secrets.Sealed{
			Ciphertext: sealed.Ciphertext,
			Nonce:      sealed.Nonce,
			KeyRef:     sealed.KeyRef,
			Cipher:     sealed.Cipher,
		}, sealed.AAD())
		if err != nil {
			return domain.Credential{}, 0, fmt.Errorf("open the stored credential for %s: %w", providerID, err)
		}

		return domain.Credential{
			ID:         sealed.ID,
			ProviderID: providerID,
			Kind:       sealed.Kind,
			Secret:     string(plaintext),
			Meta:       sealed.Meta,
		}, sealed.ID, nil
	}

	return domain.Credential{}, 0, fmt.Errorf("%w: no credential stored for %q", domain.ErrNotFound, providerID)
}

// credentialKinds is every kind a provider may hold. Iterated wherever "this
// provider's credential" is meant regardless of how it was obtained.
var credentialKinds = []string{domain.CredentialOAuthToken, domain.CredentialAPIKey}

// firstOf runs a lookup across every credential kind and returns the first hit.
func firstOf(
	ctx context.Context,
	lookup func(context.Context, string, string) (domain.SealedCredential, error),
	providerID string,
) (domain.SealedCredential, bool, error) {
	for _, kind := range credentialKinds {
		sealed, err := lookup(ctx, providerID, kind)
		if errors.Is(err, domain.ErrNotFound) {
			continue
		}
		if err != nil {
			return domain.SealedCredential{}, false, err
		}
		return sealed, true, nil
	}
	return domain.SealedCredential{}, false, nil
}

// DeleteCredential retires every live credential for a provider.
//
// Retire rather than delete: the row is the record that a credential was once in
// use, and its metadata is what explains a later "why did this stop working".
func (s *providerService) DeleteCredential(ctx context.Context, id string) error {
	if _, err := s.registry.Get(id); err != nil {
		return err
	}

	return s.tx.InTx(ctx, func(ctx context.Context) error {
		for _, kind := range credentialKinds {
			if err := s.creds.Retire(ctx, id, kind, domain.CredentialRevoked); err != nil {
				return err
			}
		}
		return nil
	})
}
