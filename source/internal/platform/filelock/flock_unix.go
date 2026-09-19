//go:build unix

package filelock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// pollInterval is how often a blocked caller retries.
//
// The lock is taken non-blocking and retried rather than taken with a blocking
// flock, because a blocking flock cannot be cancelled: the syscall would hold
// the goroutine past the deadline of the context the caller passed in. The
// interval is short enough that an uncontended handover is imperceptible and
// long enough that waiting costs nothing measurable.
const pollInterval = 15 * time.Millisecond

// dirPerm and filePerm keep the lock owner-only, like the rest of the layout.
const (
	dirPerm  = 0o700
	filePerm = 0o600
)

type flockLocker struct {
	path string
}

// New returns a Locker backed by flock on path, creating the file and its
// parent directory if they do not exist.
//
// flock is held per open file description, so each Lock call opens its own
// descriptor: two Lockers on one path exclude each other even inside a single
// process. Sharing one descriptor would make the second acquisition succeed
// immediately and guard nothing.
func New(path string) Locker { return flockLocker{path: path} }

func (l flockLocker) Lock(ctx context.Context) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(l.path), dirPerm); err != nil {
		return nil, fmt.Errorf("create the lock directory: %w", err)
	}

	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_RDWR, filePerm)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", l.path, err)
	}

	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		switch {
		case err == nil:
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
			}, nil
		case !errors.Is(err, syscall.EWOULDBLOCK):
			_ = f.Close()
			return nil, fmt.Errorf("lock %s: %w", l.path, err)
		}

		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, fmt.Errorf("lock %s: %w", l.path, ctx.Err())
		case <-time.After(pollInterval):
		}
	}
}
