package sqlite

import (
	"context"
	"time"

	"github.com/tumika/tumika/source/internal/domain"
	"github.com/tumika/tumika/source/internal/repository"
)

// LoginSessionRepo is the SQLite implementation of
// repository.LoginSessionRepository.
type LoginSessionRepo struct{ s *Store }

func NewLoginSessionRepo(s *Store) *LoginSessionRepo { return &LoginSessionRepo{s: s} }

var _ repository.LoginSessionRepository = (*LoginSessionRepo)(nil)

// Create starts a session. It returns domain.ErrConflict when the provider
// already has one in flight — the partial unique index enforces that, rather
// than a service-layer check that would race with itself.
func (r *LoginSessionRepo) Create(ctx context.Context, s domain.LoginSession) error {
	now := time.Now()
	created, updated := s.CreatedAt, s.UpdatedAt
	if created.IsZero() {
		created = now
	}
	if updated.IsZero() {
		updated = now
	}

	return mapError(r.s.writeQ(ctx).CreateLoginSession(ctx, CreateLoginSessionParams{
		ID:           s.ID,
		ProviderID:   s.ProviderID,
		State:        string(s.State),
		AuthUrl:      s.AuthURL,
		Prompt:       s.Prompt,
		ErrorCode:    s.ErrorCode,
		ErrorMessage: s.ErrorMessage,
		CredentialID: nullInt64(s.CredentialID),
		ChildPid:     nullIntFromPtr(s.ChildPID),
		Transcript:   s.Transcript,
		CreatedAt:    formatTime(created),
		UpdatedAt:    formatTime(updated),
		ExpiresAt:    formatTime(s.ExpiresAt),
	}))
}

func (r *LoginSessionRepo) Get(ctx context.Context, id string) (domain.LoginSession, error) {
	row, err := r.s.readQ(ctx).GetLoginSession(ctx, id)
	if err != nil {
		return domain.LoginSession{}, mapError(err)
	}
	return loginSessionFrom(row)
}

func (r *LoginSessionRepo) Update(ctx context.Context, s domain.LoginSession) error {
	return mapError(r.s.writeQ(ctx).UpdateLoginSession(ctx, UpdateLoginSessionParams{
		State:        string(s.State),
		AuthUrl:      s.AuthURL,
		Prompt:       s.Prompt,
		ErrorCode:    s.ErrorCode,
		ErrorMessage: s.ErrorMessage,
		CredentialID: nullInt64(s.CredentialID),
		ChildPid:     nullIntFromPtr(s.ChildPID),
		Transcript:   s.Transcript,
		UpdatedAt:    formatTime(time.Now()),
		ExpiresAt:    formatTime(s.ExpiresAt),
		ID:           s.ID,
	}))
}

// FailAllNonTerminal runs at daemon startup. A session's PTY and child process
// do not survive a restart, so a row left mid-flight describes something that no
// longer exists — leaving it would also hold the one-in-flight slot forever and
// make every future login for that provider fail with a conflict.
func (r *LoginSessionRepo) FailAllNonTerminal(ctx context.Context, reason string) (int64, error) {
	n, err := r.s.writeQ(ctx).FailAllNonTerminalLoginSessions(ctx, FailAllNonTerminalLoginSessionsParams{
		ErrorMessage: reason,
		UpdatedAt:    formatTime(time.Now()),
	})
	if err != nil {
		return 0, mapError(err)
	}
	return n, nil
}

func loginSessionFrom(row LoginSession) (domain.LoginSession, error) {
	created, err := parseTime(row.CreatedAt)
	if err != nil {
		return domain.LoginSession{}, err
	}
	updated, err := parseTime(row.UpdatedAt)
	if err != nil {
		return domain.LoginSession{}, err
	}
	expires, err := parseTime(row.ExpiresAt)
	if err != nil {
		return domain.LoginSession{}, err
	}

	return domain.LoginSession{
		ID:           row.ID,
		ProviderID:   row.ProviderID,
		State:        domain.LoginState(row.State),
		AuthURL:      row.AuthUrl,
		Prompt:       row.Prompt,
		ErrorCode:    row.ErrorCode,
		ErrorMessage: row.ErrorMessage,
		CredentialID: parseNullInt64(row.CredentialID),
		ChildPID:     parseNullIntPtr(row.ChildPid),
		Transcript:   row.Transcript,
		CreatedAt:    created,
		UpdatedAt:    updated,
		ExpiresAt:    expires,
	}, nil
}
