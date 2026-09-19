// Package filelock serialises an operation across processes with an exclusive
// advisory lock on a file.
//
// A mutex is not enough for the operations that need this: `tumika token
// rotate` runs in its own process, so two of them share nothing but the
// filesystem and the database. The lock file is the only thing both can agree
// on before either has written anything.
package filelock

import "context"

// Locker guards a critical section that spans processes.
//
// Lock blocks until the lock is held or ctx is done, and returns the release
// for the caller to defer. The lock is advisory: it excludes other callers of
// the same path, not a process that writes the guarded state directly.
type Locker interface {
	Lock(ctx context.Context) (unlock func(), err error)
}

// noop holds nothing. It is what a test uses when the behaviour under test is
// not the locking, so no file and no filesystem are involved.
type noop struct{}

// NewNoop returns a Locker that grants the lock immediately and always.
func NewNoop() Locker { return noop{} }

func (noop) Lock(context.Context) (func(), error) { return func() {}, nil }
