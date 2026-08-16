package sqlite

import (
	"context"
	"time"

	"github.com/tumika/tumika/source/internal/domain"
	"github.com/tumika/tumika/source/internal/repository"
)

// CredentialRepo is the SQLite implementation of
// repository.CredentialRepository.
//
// It stores ciphertext and never sees a plaintext secret: sealing happens in
// the service layer, above this. See
// .agents/rules/never-log-or-return-a-credential-secret.md.
type CredentialRepo struct{ s *Store }

func NewCredentialRepo(s *Store) *CredentialRepo { return &CredentialRepo{s: s} }

var _ repository.CredentialRepository = (*CredentialRepo)(nil)

func (r *CredentialRepo) GetLive(ctx context.Context, providerID, kind string) (domain.SealedCredential, error) {
	row, err := r.s.readQ(ctx).GetLiveCredential(ctx, GetLiveCredentialParams{
		ProviderID: providerID,
		Kind:       kind,
	})
	if err != nil {
		return domain.SealedCredential{}, mapError(err)
	}
	return credentialFrom(row)
}

func (r *CredentialRepo) ListLive(ctx context.Context) ([]domain.SealedCredential, error) {
	rows, err := r.s.readQ(ctx).ListLiveCredentials(ctx)
	if err != nil {
		return nil, mapError(err)
	}

	out := make([]domain.SealedCredential, 0, len(rows))
	for _, row := range rows {
		c, err := credentialFrom(ProviderCredential(row))
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// Insert stores a newly sealed credential.
//
// It returns domain.ErrConflict when a live credential already exists for this
// provider and kind — the partial unique index rejects it. That is deliberate:
// replacing a credential means deciding what happens to the old one, and Retire
// is where that decision is expressed. Silently overwriting would discard the
// record of what was previously in use.
func (r *CredentialRepo) Insert(ctx context.Context, c domain.SealedCredential) (int64, error) {
	now := time.Now()
	created, updated := c.CreatedAt, c.UpdatedAt
	if created.IsZero() {
		created = now
	}
	if updated.IsZero() {
		updated = now
	}

	cipher := c.Cipher
	if cipher == "" {
		cipher = "aes-256-gcm"
	}

	id, err := r.s.writeQ(ctx).InsertCredential(ctx, InsertCredentialParams{
		ProviderID:       c.ProviderID,
		Kind:             c.Kind,
		Ciphertext:       c.Ciphertext,
		Nonce:            c.Nonce,
		KeyRef:           c.KeyRef,
		Cipher:           cipher,
		Hint:             c.Meta.Hint,
		AccountEmail:     c.Meta.AccountEmail,
		Status:           c.Meta.Status,
		IssuedAt:         nullTime(c.Meta.IssuedAt),
		ExpiresAt:        nullTime(c.Meta.ExpiresAt),
		ExpiryIsEstimate: boolToInt(c.Meta.ExpiryIsEstimate),
		LastVerifiedAt:   nullTime(c.Meta.LastVerifiedAt),
		LastVerifyError:  c.Meta.LastVerifyError,
		CreatedAt:        formatTime(created),
		UpdatedAt:        formatTime(updated),
	})
	if err != nil {
		return 0, mapError(err)
	}
	return id, nil
}

func (r *CredentialRepo) UpdateStatus(ctx context.Context, id int64, status domain.CredentialStatus, verifyErr string) (bool, error) {
	rows, err := r.s.writeQ(ctx).UpdateCredentialStatus(ctx, UpdateCredentialStatusParams{
		Status:          string(status),
		LastVerifyError: verifyErr,
		UpdatedAt:       formatTime(time.Now()),
		ID:              id,
	})
	if err != nil {
		return false, mapError(err)
	}
	return rows > 0, nil
}

func (r *CredentialRepo) UpdateMeta(ctx context.Context, id int64, meta domain.CredentialMeta) (bool, error) {
	rows, err := r.s.writeQ(ctx).UpdateCredentialMeta(ctx, UpdateCredentialMetaParams{
		Hint:             meta.Hint,
		AccountEmail:     meta.AccountEmail,
		IssuedAt:         nullTime(meta.IssuedAt),
		ExpiresAt:        nullTime(meta.ExpiresAt),
		ExpiryIsEstimate: boolToInt(meta.ExpiryIsEstimate),
		LastVerifiedAt:   nullTime(meta.LastVerifiedAt),
		UpdatedAt:        formatTime(time.Now()),
		ID:               id,
	})
	if err != nil {
		return false, mapError(err)
	}
	return rows > 0, nil
}

// Retire frees the slot the partial unique index guards, moving every live
// credential for a provider and kind to a terminal status. The rows stay: what
// was tried is part of the record.
func (r *CredentialRepo) Retire(ctx context.Context, providerID, kind string, status domain.CredentialStatus) error {
	return mapError(r.s.writeQ(ctx).RetireCredentials(ctx, RetireCredentialsParams{
		Status:     string(status),
		UpdatedAt:  formatTime(time.Now()),
		ProviderID: providerID,
		Kind:       kind,
	}))
}

func (r *CredentialRepo) Delete(ctx context.Context, id int64) error {
	return mapError(r.s.writeQ(ctx).DeleteCredential(ctx, id))
}

func credentialFrom(row ProviderCredential) (domain.SealedCredential, error) {
	created, err := parseTime(row.CreatedAt)
	if err != nil {
		return domain.SealedCredential{}, err
	}
	updated, err := parseTime(row.UpdatedAt)
	if err != nil {
		return domain.SealedCredential{}, err
	}
	issued, err := parseNullTime(row.IssuedAt)
	if err != nil {
		return domain.SealedCredential{}, err
	}
	expires, err := parseNullTime(row.ExpiresAt)
	if err != nil {
		return domain.SealedCredential{}, err
	}
	verified, err := parseNullTime(row.LastVerifiedAt)
	if err != nil {
		return domain.SealedCredential{}, err
	}

	return domain.SealedCredential{
		ID:         row.ID,
		ProviderID: row.ProviderID,
		Kind:       row.Kind,
		Ciphertext: row.Ciphertext,
		Nonce:      row.Nonce,
		KeyRef:     row.KeyRef,
		Cipher:     row.Cipher,
		Meta: domain.CredentialMeta{
			Hint:             row.Hint,
			AccountEmail:     row.AccountEmail,
			Status:           row.Status,
			IssuedAt:         issued,
			ExpiresAt:        expires,
			ExpiryIsEstimate: intToBool(row.ExpiryIsEstimate),
			LastVerifiedAt:   verified,
			LastVerifyError:  row.LastVerifyError,
		},
		CreatedAt: created,
		UpdatedAt: updated,
	}, nil
}
