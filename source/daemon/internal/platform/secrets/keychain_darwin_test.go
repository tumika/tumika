//go:build darwin

package secrets

import (
	"bytes"
	"os"
	"testing"

	"github.com/zalando/go-keyring"
)

// KeychainTestEnv opts a run in to exercising the REAL login Keychain.
//
// Off by default, and deliberately so: this test creates and deletes an entry in
// the Keychain of whoever runs it, which is not a side effect an ordinary
// `go test ./...` should have. Run it with TUMIKA_TEST_KEYCHAIN=1 when changing
// the keychain backend.
const KeychainTestEnv = "TUMIKA_TEST_KEYCHAIN"

func TestKeychainKeyStoreRoundTrip(t *testing.T) {
	if os.Getenv(KeychainTestEnv) == "" {
		t.Skipf("set %s=1 to exercise the real login Keychain", KeychainTestEnv)
	}

	// Leave the machine as we found it, whichever way the test goes.
	existing, hadExisting := keyring.Get(keychainService, keychainAccount)
	t.Cleanup(func() {
		if hadExisting == nil {
			_ = keyring.Set(keychainService, keychainAccount, existing)
			return
		}
		_ = keyring.Delete(keychainService, keychainAccount)
	})

	if hadExisting == nil {
		if err := keyring.Delete(keychainService, keychainAccount); err != nil {
			t.Fatalf("clearing the existing entry: %v", err)
		}
	}

	store, err := newKeychainKeyStore()
	if err != nil {
		t.Fatalf("newKeychainKeyStore: %v", err)
	}
	if store.Backend() != BackendKeychain {
		t.Errorf("Backend = %q", store.Backend())
	}

	first, err := store.Key()
	if err != nil {
		t.Fatalf("Key: %v", err)
	}
	if len(first) != KeySize {
		t.Fatalf("key is %d bytes, want %d", len(first), KeySize)
	}

	// Re-opening must reuse the key. Minting a new one per start would orphan
	// every stored credential.
	again, err := newKeychainKeyStore()
	if err != nil {
		t.Fatalf("re-open: %v", err)
	}
	second, err := again.Key()
	if err != nil {
		t.Fatalf("Key: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Error("re-opening the keychain store produced a different key")
	}
}
