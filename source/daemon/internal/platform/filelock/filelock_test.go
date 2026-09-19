package filelock_test

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tumika/tumika/source/daemon/internal/platform/filelock"
)

func lockPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "guard.lock")
}

// Two Lockers on one path exclude each other inside a single process. flock is
// held per open file description, so an implementation that shared one
// descriptor would pass every cross-process test and guard nothing here.
func TestTwoLockersOnOnePathExcludeEachOther(t *testing.T) {
	path := lockPath(t)
	first, second := filelock.New(path), filelock.New(path)

	unlock, err := first.Lock(t.Context())
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}

	var acquired atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		release, err := second.Lock(context.Background())
		if err != nil {
			t.Errorf("second Lock: %v", err)
			return
		}
		acquired.Store(true)
		release()
	}()

	time.Sleep(100 * time.Millisecond)
	if acquired.Load() {
		t.Fatal("the second Locker took the lock while the first held it")
	}

	unlock()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the second Locker never took the lock after it was released")
	}
	if !acquired.Load() {
		t.Fatal("the second Locker reported no error and never acquired")
	}
}

// Unlock releases the lock, so the same Locker can take it again.
func TestUnlockReleases(t *testing.T) {
	locker := filelock.New(lockPath(t))

	for range 3 {
		unlock, err := locker.Lock(t.Context())
		if err != nil {
			t.Fatalf("Lock: %v", err)
		}
		unlock()
	}
}

// A blocked caller honours its context rather than waiting forever, which is
// what makes a wedged holder a reported failure instead of a hung command.
func TestLockRespectsContextCancellation(t *testing.T) {
	path := lockPath(t)

	unlock, err := filelock.New(path).Lock(t.Context())
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}
	defer unlock()

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	if _, err := filelock.New(path).Lock(ctx); err == nil {
		t.Fatal("Lock succeeded while the lock was held elsewhere")
	}
	if waited := time.Since(start); waited > 5*time.Second {
		t.Fatalf("Lock waited %s past its context", waited)
	}
}

func TestLockFileIsOwnerOnly(t *testing.T) {
	path := lockPath(t)

	unlock, err := filelock.New(path).Lock(t.Context())
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}
	defer unlock()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("lock file mode is %#o, want owner-only", perm)
	}
}

// The no-op grants the lock unconditionally: it is what a test that is not
// exercising the locking gets.
func TestNoopAlwaysGrants(t *testing.T) {
	locker := filelock.NewNoop()

	first, err := locker.Lock(t.Context())
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}
	second, err := locker.Lock(t.Context())
	if err != nil {
		t.Fatalf("second Lock: %v", err)
	}
	first()
	second()
}
