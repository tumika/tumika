// Package domain holds the types shared across every layer.
//
// It imports nothing of ours — `depguard` enforces that — which is what lets
// every other layer depend on it without creating a cycle. Anything domain
// would need from another package belongs in domain itself.
package domain

import "errors"

// Sentinel errors cross layer boundaries. A repository returns ErrNotFound, a
// service decides what that means, and the API maps it onto a status code and an
// error envelope. That chain only works if the errors are values rather than
// strings, so always wrap with %w.
var (
	// ErrNotFound is returned by a repository when a row does not exist.
	ErrNotFound = errors.New("not found")

	// ErrConflict is returned when a write violates a uniqueness constraint —
	// most usefully the partial index that permits only one live credential per
	// provider and kind.
	ErrConflict = errors.New("conflict")

	// ErrCredentialInvalid is returned when a credential fails verification
	// against its provider. It is a real answer, not a transport failure.
	ErrCredentialInvalid = errors.New("credential invalid")

	// ErrInstallUnsupported is returned for a provider whose driver does not
	// implement Installer.
	ErrInstallUnsupported = errors.New("install unsupported")

	// ErrInteractiveAuthUnsupported is returned when a login session is
	// requested for a provider with no interactive auth method.
	ErrInteractiveAuthUnsupported = errors.New("interactive auth unsupported")

	// ErrInteractiveAuthRequired is the mirror image: a secret was submitted
	// directly to a provider that only hands one over through a login session.
	ErrInteractiveAuthRequired = errors.New("this provider requires an interactive login")

	// ErrSchemaTooNew is returned when the database has been migrated by a newer
	// binary than this one. tumika refuses to start rather than operate against
	// a schema it does not understand — the safety net for a rollback after a
	// failed update (ADR-0003).
	ErrSchemaTooNew = errors.New("database schema is newer than this binary supports")
)
