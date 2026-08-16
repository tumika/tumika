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
	if cred.ProviderID == "" {
		return domain.CredentialMeta{}, fmt.Errorf("%w: no provider", domain.ErrCredentialInvalid)
	}
	if cred.Secret == "" {
		return domain.CredentialMeta{}, fmt.Errorf("%w: no secret", domain.ErrCredentialInvalid)
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

	var id int64
	err = s.tx.InTx(ctx, func(ctx context.Context) error {
		// The previous credential is retired, not deleted: what was tried is
		// part of the record, and the partial unique index needs the slot freed.
		if err := s.creds.Retire(ctx, cred.ProviderID, cred.Kind, domain.CredentialRevoked); err != nil {
			return err
		}
		var err error
		id, err = s.creds.Insert(ctx, sealed)
		return err
	})
	if err != nil {
		return domain.CredentialMeta{}, err
	}

	meta, err := s.verify(ctx, cred.ProviderID, id, cred)
	if err != nil {
		// The credential is stored and unverified. That is a real state, not a
		// failure to store — the monitor will try again — so report the metadata
		// alongside the error rather than pretending nothing happened.
		return sealed.Meta, err
	}
	return meta, nil
}

func (s *providerService) VerifyCredential(ctx context.Context, id string) (domain.CredentialMeta, error) {
	if _, err := s.registry.Get(id); err != nil {
		return domain.CredentialMeta{}, err
	}

	cred, sealedID, err := s.openLive(ctx, id)
	if err != nil {
		return domain.CredentialMeta{}, err
	}
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
	checker, err := s.registry.HealthChecker(providerID)
	if err != nil {
		return domain.CredentialMeta{}, err
	}

	meta, verifyErr := checker.Verify(ctx, cred)
	if verifyErr != nil {
		return meta, verifyErr
	}

	if meta.Status == "" {
		meta.Status = string(domain.CredentialUnverified)
	}

	err = s.tx.InTx(ctx, func(ctx context.Context) error {
		if err := s.creds.UpdateMeta(ctx, sealedID, meta); err != nil {
			return err
		}
		return s.creds.UpdateStatus(ctx, sealedID, domain.CredentialStatus(meta.Status), meta.LastVerifyError)
	})
	if err != nil {
		return meta, err
	}

	if meta.Status == string(domain.CredentialInvalid) {
		return meta, fmt.Errorf("%w: %s", domain.ErrCredentialInvalid, meta.LastVerifyError)
	}
	return meta, nil
}

// openLive unseals the live credential for a provider.
func (s *providerService) openLive(ctx context.Context, providerID string) (domain.Credential, int64, error) {
	for _, kind := range []string{domain.CredentialOAuthToken, domain.CredentialAPIKey} {
		sealed, err := s.creds.GetLive(ctx, providerID, kind)
		if errors.Is(err, domain.ErrNotFound) {
			continue
		}
		if err != nil {
			return domain.Credential{}, 0, err
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

// DeleteCredential retires every live credential for a provider.
//
// Retire rather than delete: the row is the record that a credential was once in
// use, and its metadata is what explains a later "why did this stop working".
func (s *providerService) DeleteCredential(ctx context.Context, id string) error {
	if _, err := s.registry.Get(id); err != nil {
		return err
	}

	return s.tx.InTx(ctx, func(ctx context.Context) error {
		for _, kind := range []string{domain.CredentialOAuthToken, domain.CredentialAPIKey} {
			if err := s.creds.Retire(ctx, id, kind, domain.CredentialRevoked); err != nil {
				return err
			}
		}
		return nil
	})
}
