package sqlite

import (
	"context"
	"time"

	"github.com/tumika/tumika/source/internal/domain"
	"github.com/tumika/tumika/source/internal/repository"
)

// ConfigRepo is the SQLite implementation of repository.ConfigRepository.
type ConfigRepo struct{ s *Store }

// NewConfigRepo returns a ConfigRepo bound to the store.
func NewConfigRepo(s *Store) *ConfigRepo { return &ConfigRepo{s: s} }

// Compile-time proof the implementation still satisfies the interface. Without
// this, a signature change in the interface fails at the composition root
// instead of here, which is much further from the cause.
var _ repository.ConfigRepository = (*ConfigRepo)(nil)

func (r *ConfigRepo) Get(ctx context.Context, key string) (domain.Setting, error) {
	row, err := r.s.readQ(ctx).GetSetting(ctx, key)
	if err != nil {
		return domain.Setting{}, mapError(err)
	}
	return settingFrom(row)
}

func (r *ConfigRepo) List(ctx context.Context) ([]domain.Setting, error) {
	rows, err := r.s.readQ(ctx).ListSettings(ctx)
	if err != nil {
		return nil, mapError(err)
	}

	out := make([]domain.Setting, 0, len(rows))
	for _, row := range rows {
		s, err := settingFrom(row)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

func (r *ConfigRepo) Upsert(ctx context.Context, s domain.Setting) error {
	updated := s.UpdatedAt
	if updated.IsZero() {
		updated = time.Now()
	}
	return mapError(r.s.writeQ(ctx).UpsertSetting(ctx, UpsertSettingParams{
		Key:       s.Key,
		Value:     string(s.Value),
		UpdatedAt: formatTime(updated),
	}))
}

// Delete removes a setting. Deleting a key that is not there is not an error:
// the caller asked for it to be gone, and it is.
func (r *ConfigRepo) Delete(ctx context.Context, key string) error {
	return mapError(r.s.writeQ(ctx).DeleteSetting(ctx, key))
}

func settingFrom(row Setting) (domain.Setting, error) {
	updated, err := parseTime(row.UpdatedAt)
	if err != nil {
		return domain.Setting{}, err
	}
	return domain.Setting{
		Key:       row.Key,
		Value:     []byte(row.Value),
		UpdatedAt: updated,
	}, nil
}
