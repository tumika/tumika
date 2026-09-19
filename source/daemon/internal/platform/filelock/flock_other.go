//go:build !unix

package filelock

import (
	"context"
	"errors"
)

// ErrUnsupported reports that the platform has no advisory file locking tumika
// knows how to use.
var ErrUnsupported = errors.New("advisory file locking is unsupported on this platform")

type unsupported struct{}

// New returns a Locker that refuses.
//
// Locking is implemented with flock, which the release targets (linux and
// darwin) all have. Refusing is the honest answer anywhere else: handing back a
// lock that guards nothing would let two processes interleave a critical
// section while both believed they were alone.
func New(string) Locker { return unsupported{} }

func (unsupported) Lock(context.Context) (func(), error) { return nil, ErrUnsupported }
