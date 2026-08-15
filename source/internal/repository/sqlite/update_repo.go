package sqlite

import (
	"context"
	"database/sql"
	"time"

	"github.com/tumika/tumika/source/internal/domain"
	"github.com/tumika/tumika/source/internal/repository"
)

// UpdateStateRepo is the SQLite implementation of
// repository.UpdateStateRepository. It reads and writes a single row, seeded by
// the initial migration and guarded by CHECK (id = 1).
type UpdateStateRepo struct{ s *Store }

func NewUpdateStateRepo(s *Store) *UpdateStateRepo { return &UpdateStateRepo{s: s} }

var _ repository.UpdateStateRepository = (*UpdateStateRepo)(nil)

func (r *UpdateStateRepo) Get(ctx context.Context) (domain.UpdateState, error) {
	row, err := r.s.readQ(ctx).GetUpdateState(ctx)
	if err != nil {
		return domain.UpdateState{}, mapError(err)
	}
	return updateStateFrom(row.Status, row.FromVersion, row.ToVersion, row.BootAttempts, row.StartedAt, row.UpdatedAt)
}

func (r *UpdateStateRepo) Put(ctx context.Context, s domain.UpdateState) error {
	updated := s.UpdatedAt
	if updated.IsZero() {
		updated = time.Now()
	}
	return mapError(r.s.writeQ(ctx).PutUpdateState(ctx, PutUpdateStateParams{
		Status:       string(s.Status),
		FromVersion:  s.FromVersion,
		ToVersion:    s.ToVersion,
		BootAttempts: int64(s.BootAttempts),
		StartedAt:    nullTime(s.StartedAt),
		UpdatedAt:    formatTime(updated),
	}))
}

// IncrementBootAttempts bumps the counter and returns the new state in one
// statement. Read-modify-write would be wrong here specifically: this runs on
// every boot of a pending update, which is exactly the moment the process is
// most likely to die partway through — and a lost increment means a binary that
// cannot boot never reaches the rollback threshold.
func (r *UpdateStateRepo) IncrementBootAttempts(ctx context.Context) (domain.UpdateState, error) {
	row, err := r.s.writeQ(ctx).IncrementBootAttempts(ctx, formatTime(time.Now()))
	if err != nil {
		return domain.UpdateState{}, mapError(err)
	}
	return updateStateFrom(row.Status, row.FromVersion, row.ToVersion, row.BootAttempts, row.StartedAt, row.UpdatedAt)
}

func updateStateFrom(
	status, fromVersion, toVersion string,
	bootAttempts int64,
	startedAt sql.NullString,
	updatedAt string,
) (domain.UpdateState, error) {
	updated, err := parseTime(updatedAt)
	if err != nil {
		return domain.UpdateState{}, err
	}
	started, err := parseNullTime(startedAt)
	if err != nil {
		return domain.UpdateState{}, err
	}
	return domain.UpdateState{
		Status:       domain.UpdateStatus(status),
		FromVersion:  fromVersion,
		ToVersion:    toVersion,
		BootAttempts: int(bootAttempts),
		StartedAt:    started,
		UpdatedAt:    updated,
	}, nil
}
