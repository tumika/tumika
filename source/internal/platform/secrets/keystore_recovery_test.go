package secrets_test

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/tumika/tumika/source/internal/platform/secrets"
)

// A crash between create and write used to leave an empty key file that no later
// start could interpret — the daemon refused to boot with "no master key
// available" and no hint that deleting it was the fix.
func TestEmptyKeyFileIsTreatedAsAbsent(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "master.key")
	if err := os.WriteFile(keyFile, nil, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	store, err := secrets.NewFileKeyStore(keyFile)
	if err != nil {
		t.Fatalf("an empty key file should be replaced, not fatal: %v", err)
	}
	key, err := store.Key()
	if err != nil || len(key) != secrets.KeySize {
		t.Errorf("Key() = %d bytes, %v", len(key), err)
	}
}

// Two processes initialising custody at once — a supervisor restarting the
// daemon mid-first-run, or a CLI command and the daemon together — must agree on
// one key. The loser of the create race has to LOAD the winner's key; returning
// EEXIST would report a race as if it were corruption, and minting a second key
// would orphan whatever the winner had already sealed.
func TestConcurrentFirstRunAgreesOnOneKey(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "master.key")

	const racers = 8
	keys := make([][]byte, racers)
	errs := make([]error, racers)

	var wg sync.WaitGroup
	start := make(chan struct{})

	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // release them together, to make the race likely

			store, err := secrets.NewFileKeyStore(keyFile)
			if err != nil {
				errs[i] = err
				return
			}
			keys[i], errs[i] = store.Key()
		}()
	}

	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("racer %d failed to initialise custody: %v", i, err)
		}
	}
	for i := 1; i < racers; i++ {
		if !bytes.Equal(keys[0], keys[i]) {
			t.Fatalf("racer %d got a different key; credentials sealed by one would be unreadable by the other", i)
		}
	}
}
