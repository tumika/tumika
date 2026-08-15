package sqlite

import (
	"context"
	"time"

	"github.com/tumika/tumika/source/internal/domain"
	"github.com/tumika/tumika/source/internal/repository"
)

// ProviderRepo is the SQLite implementation of repository.ProviderRepository.
type ProviderRepo struct{ s *Store }

func NewProviderRepo(s *Store) *ProviderRepo { return &ProviderRepo{s: s} }

var _ repository.ProviderRepository = (*ProviderRepo)(nil)

func (r *ProviderRepo) Get(ctx context.Context, id string) (domain.Provider, error) {
	row, err := r.s.readQ(ctx).GetProvider(ctx, id)
	if err != nil {
		return domain.Provider{}, mapError(err)
	}
	return providerFrom(row)
}

func (r *ProviderRepo) List(ctx context.Context) ([]domain.Provider, error) {
	rows, err := r.s.readQ(ctx).ListProviders(ctx)
	if err != nil {
		return nil, mapError(err)
	}

	out := make([]domain.Provider, 0, len(rows))
	for _, row := range rows {
		p, err := providerFrom(row)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// Upsert seeds or refreshes a provider. The registry calls it at every boot, so
// it must be idempotent.
//
// The query deliberately does not overwrite `enabled`: display name, kind and
// config come from the driver and should track it, but whether a provider is
// enabled is the operator's decision, and a restart must not silently undo it.
func (r *ProviderRepo) Upsert(ctx context.Context, p domain.Provider) error {
	now := time.Now()
	created, updated := p.CreatedAt, p.UpdatedAt
	if created.IsZero() {
		created = now
	}
	if updated.IsZero() {
		updated = now
	}

	config := p.Config
	if len(config) == 0 {
		config = []byte("{}")
	}

	return mapError(r.s.writeQ(ctx).UpsertProvider(ctx, UpsertProviderParams{
		ID:          p.ID,
		DisplayName: p.DisplayName,
		Kind:        p.Kind,
		Enabled:     boolToInt(p.Enabled),
		Config:      string(config),
		CreatedAt:   formatTime(created),
		UpdatedAt:   formatTime(updated),
	}))
}

func (r *ProviderRepo) SetEnabled(ctx context.Context, id string, enabled bool) error {
	return mapError(r.s.writeQ(ctx).SetProviderEnabled(ctx, SetProviderEnabledParams{
		Enabled:   boolToInt(enabled),
		UpdatedAt: formatTime(time.Now()),
		ID:        id,
	}))
}

func providerFrom(row Provider) (domain.Provider, error) {
	created, err := parseTime(row.CreatedAt)
	if err != nil {
		return domain.Provider{}, err
	}
	updated, err := parseTime(row.UpdatedAt)
	if err != nil {
		return domain.Provider{}, err
	}
	return domain.Provider{
		ID:          row.ID,
		DisplayName: row.DisplayName,
		Kind:        row.Kind,
		Enabled:     intToBool(row.Enabled),
		Config:      []byte(row.Config),
		CreatedAt:   created,
		UpdatedAt:   updated,
	}, nil
}
