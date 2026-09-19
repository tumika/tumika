// Package tokencustody hands the daemon's API token to the platform's own
// secret store, so an operator can retrieve it without tumika keeping a second
// copy of its own.
//
// Only the token's SHA-256 is persisted in the database, so a token that is not
// written somewhere at the moment it is minted is unrecoverable. Custody is
// therefore a platform concern rather than a service one: what is available
// differs per OS, and where nothing is available the honest answer is to store
// nothing at all.
package tokencustody

import "context"

// Custodian stores the daemon's API token in the platform's secret store.
//
// Store is write-only by design: nothing in tumika reads the token back, so
// there is no Get. An implementation that cannot store anything returns nil —
// the caller printed the token already, and a missing keystore is not a reason
// to fail an install.
type Custodian interface {
	Store(ctx context.Context, token string) error
}

// noop stores nothing. It is what every platform without a supported keystore
// gets, and what tests use so a run never touches a real store.
type noop struct{}

// NewNoop returns a Custodian that stores nothing and always succeeds.
func NewNoop() Custodian { return noop{} }

func (noop) Store(context.Context, string) error { return nil }
