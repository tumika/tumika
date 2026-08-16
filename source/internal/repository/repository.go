// Package repository is layer 3: data access, and nothing else.
//
// A repository translates between the database and domain types. It contains no
// business rules, returns domain types rather than rows, and never calls a
// service — `depguard` enforces the import half of that. Each interface here has
// exactly one owning service; when a service needs data owned by another, it
// calls that service. See .agents/rules/a-repository-has-exactly-one-owning-service.md.
//
// | Service        | Owns                                      |
// |----------------|-------------------------------------------|
// | ConfigService  | ConfigRepository                          |
// | ProviderService| ProviderRepository, CredentialRepository  |
// | LoginService   | LoginSessionRepository                    |
// | UpdateService  | UpdateStateRepository                     |
package repository

import (
	"context"

	"github.com/tumika/tumika/source/internal/domain"
)

// Txer opens a transaction boundary.
//
// Transactions are owned by the service layer, so that one service method is
// one unit of work. A service that needs several writes to land together takes
// a Txer and runs them inside InTx; the repositories it calls join that
// transaction automatically, because the implementation carries it on the
// context.
//
// Carrying it on the context is a deliberate trade. The alternative — handing
// each repository an explicit transaction handle — would put a database type in
// the signature of every repository method, which is exactly the leak the
// layering exists to prevent, and would force a service to be given the whole
// repository set rather than only the repositories it owns.
type Txer interface {
	// InTx runs fn inside a single write transaction, committing if it returns
	// nil and rolling back otherwise. Nested calls join the existing
	// transaction rather than opening a second one, because SQLite has a single
	// writer.
	InTx(ctx context.Context, fn func(ctx context.Context) error) error
}

// ConfigRepository stores generic key/value settings. Owned by ConfigService.
type ConfigRepository interface {
	// Get returns the setting for key, or domain.ErrNotFound.
	Get(ctx context.Context, key string) (domain.Setting, error)
	// List returns every setting, ordered by key.
	List(ctx context.Context) ([]domain.Setting, error)
	// Upsert inserts or replaces a setting and stamps updated_at.
	Upsert(ctx context.Context, s domain.Setting) error
	// Delete removes a setting. Deleting an absent key is not an error —
	// the caller asked for it to be gone, and it is.
	Delete(ctx context.Context, key string) error
}

// ProviderRepository stores the mutable half of a provider. Owned by
// ProviderService. The immutable half — the descriptor — comes from the driver
// registry, not from here.
type ProviderRepository interface {
	Get(ctx context.Context, id string) (domain.Provider, error)
	List(ctx context.Context) ([]domain.Provider, error)
	// Upsert seeds or updates a provider. The registry calls this at boot for
	// every driver it knows about.
	Upsert(ctx context.Context, p domain.Provider) error
	SetEnabled(ctx context.Context, id string, enabled bool) error
}

// CredentialRepository stores sealed credentials. Owned by ProviderService —
// and by nothing else. LoginService reaches credentials through
// ProviderService.StoreCredential, which is where the sealing AAD and the
// verify-before-active rule live.
type CredentialRepository interface {
	// GetLive returns the one live credential for a provider and kind — the
	// row the partial unique index permits — or domain.ErrNotFound.
	GetLive(ctx context.Context, providerID, kind string) (domain.SealedCredential, error)
	// GetLatest returns the most recent credential for a provider and kind that
	// the operator has not revoked, whatever its status.
	//
	// GetLive excludes 'invalid' and 'expired' because they must not be USED;
	// this exists because they must still be re-CHECKABLE. A key rejected by a
	// provider-side hiccup would otherwise be condemned permanently.
	GetLatest(ctx context.Context, providerID, kind string) (domain.SealedCredential, error)
	// ListLive returns every live credential, for the credential monitor and
	// for /v1/health.
	ListLive(ctx context.Context) ([]domain.SealedCredential, error)
	// Insert stores a newly sealed credential and returns its ID. It fails with
	// domain.ErrConflict if a live credential already exists for that provider
	// and kind; retiring the previous one is the caller's decision, not a
	// silent overwrite.
	Insert(ctx context.Context, c domain.SealedCredential) (int64, error)
	// UpdateStatus records the outcome of a verification, and reports whether
	// the row was still live.
	//
	// Verification runs outside a transaction — it is a network call — so the
	// row may have been retired or replaced meanwhile. applied=false means the
	// verdict arrived too late and was discarded, which is a normal outcome
	// rather than an error.
	UpdateStatus(ctx context.Context, id int64, status domain.CredentialStatus, verifyErr string) (applied bool, err error)
	// UpdateMeta merges what verification learned into the stored metadata,
	// leaving fields the driver did not report untouched.
	UpdateMeta(ctx context.Context, id int64, meta domain.CredentialMeta) (applied bool, err error)
	// Retire moves every live credential for a provider and kind to a terminal
	// status, freeing the slot the partial unique index guards.
	Retire(ctx context.Context, providerID, kind string, status domain.CredentialStatus) error
	Delete(ctx context.Context, id int64) error
}

// LoginSessionRepository stores interactive login sessions. Owned by
// LoginService.
type LoginSessionRepository interface {
	Create(ctx context.Context, s domain.LoginSession) error
	Get(ctx context.Context, id string) (domain.LoginSession, error)
	Update(ctx context.Context, s domain.LoginSession) error
	// FailAllNonTerminal marks every unfinished session as failed. It runs at
	// daemon startup: a session's PTY and child process cannot survive a
	// restart, so a row left mid-flight is describing something that no longer
	// exists.
	FailAllNonTerminal(ctx context.Context, reason string) (int64, error)
}

// UpdateStateRepository stores the single row tracking a self-update across the
// process restart that completes it. Owned by UpdateService.
type UpdateStateRepository interface {
	Get(ctx context.Context) (domain.UpdateState, error)
	Put(ctx context.Context, s domain.UpdateState) error
	// IncrementBootAttempts bumps the counter and returns the new state. It is
	// a single statement rather than a read-modify-write because it runs on
	// every boot of a pending update, which is precisely when the process may
	// be dying partway through.
	IncrementBootAttempts(ctx context.Context) (domain.UpdateState, error)
}
