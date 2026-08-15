//go:build !darwin

package secrets

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Off macOS there is no keychain to try, and the stub must say so rather than
// returning a nil store the caller would dereference.
func TestKeychainIsUnavailableOffDarwin(t *testing.T) {
	store, err := newKeychainKeyStore()
	if !errors.Is(err, errKeychainUnavailable) {
		t.Errorf("newKeychainKeyStore() = %v, want errKeychainUnavailable", err)
	}
	if store != nil {
		t.Error("a store was returned alongside an error")
	}
}

// With no override and no keychain, selection lands on the file — which is also
// the container path.
func TestOpenKeyStoreFallsBackToTheFile(t *testing.T) {
	t.Setenv(MasterKeyEnv, "")
	keyFile := filepath.Join(t.TempDir(), "master.key")

	store, err := OpenKeyStore(keyFile)
	if err != nil {
		t.Fatalf("OpenKeyStore: %v", err)
	}
	if store.Backend() != BackendFile {
		t.Errorf("Backend = %q, want %q", store.Backend(), BackendFile)
	}

	info, err := os.Stat(keyFile)
	if err != nil {
		t.Fatalf("the key file was not created: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("key file mode = %#o, want 0600", perm)
	}
}
