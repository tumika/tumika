package secrets_test

import (
	"bytes"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tumika/tumika/source/internal/platform/secrets"
)

// aad mirrors what a credential row supplies: provider_id|kind.
func aad(providerID, kind string) []byte { return []byte(providerID + "|" + kind) }

// sealerFor builds a sealer over a file key store in a temp directory.
//
// Deliberately NewFileKeyStore rather than OpenKeyStore: on a Mac, selection
// prefers the login Keychain, so these tests would both share one key — making
// TestAnotherKeyCannotOpenIt pass a foreign row — and write an entry into the
// developer's real Keychain. A test must not have that side effect on the
// machine running it.
func sealerFor(t *testing.T) secrets.Sealer {
	t.Helper()

	store, err := secrets.NewFileKeyStore(filepath.Join(t.TempDir(), "master.key"))
	if err != nil {
		t.Fatalf("NewFileKeyStore: %v", err)
	}
	sealer, err := secrets.New(store)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return sealer
}

func TestSealOpenRoundTrip(t *testing.T) {
	sealer := sealerFor(t)
	secret := []byte("sk-ant-oat01-" + strings.Repeat("A", 64))

	sealed, err := sealer.Seal(secret, aad("claude-code", "oauth_token"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	if bytes.Contains(sealed.Ciphertext, secret) {
		t.Fatal("the plaintext is present in the ciphertext")
	}
	if sealed.Cipher != secrets.CipherAESGCM {
		t.Errorf("Cipher = %q", sealed.Cipher)
	}
	if sealed.KeyRef == "" {
		t.Error("KeyRef must record which custody sealed the row")
	}
	if len(sealed.Nonce) == 0 {
		t.Error("Nonce must be recorded")
	}

	opened, err := sealer.Open(sealed, aad("claude-code", "oauth_token"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(opened, secret) {
		t.Error("the opened plaintext does not match")
	}
}

// The AAD binds ciphertext to its row. Without it, a row copied from one
// provider to another would decrypt cleanly and tumika would authenticate to one
// provider with another's credential — a bug that looks like a mysterious 401.
func TestAADBindsCiphertextToItsRow(t *testing.T) {
	sealer := sealerFor(t)
	secret := []byte("a-credential")

	sealed, err := sealer.Seal(secret, aad("claude-code", "oauth_token"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	for _, wrong := range [][]byte{
		aad("anthropic-api", "oauth_token"), // transplanted to another provider
		aad("claude-code", "api_key"),       // transplanted to another kind
		nil,                                 // AAD dropped entirely
		{},
	} {
		if _, err := sealer.Open(sealed, wrong); !errors.Is(err, secrets.ErrCorrupt) {
			t.Errorf("Open with AAD %q = %v, want ErrCorrupt", wrong, err)
		}
	}
}

func TestTamperingIsDetected(t *testing.T) {
	sealer := sealerFor(t)
	binding := aad("claude-code", "oauth_token")

	sealed, err := sealer.Seal([]byte("a-credential"), binding)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	tests := map[string]func(s secrets.Sealed) secrets.Sealed{
		"flipped ciphertext bit": func(s secrets.Sealed) secrets.Sealed {
			c := bytes.Clone(s.Ciphertext)
			c[0] ^= 0x01
			s.Ciphertext = c
			return s
		},
		"flipped nonce bit": func(s secrets.Sealed) secrets.Sealed {
			n := bytes.Clone(s.Nonce)
			n[0] ^= 0x01
			s.Nonce = n
			return s
		},
		"truncated ciphertext": func(s secrets.Sealed) secrets.Sealed {
			s.Ciphertext = s.Ciphertext[:len(s.Ciphertext)-1]
			return s
		},
		"wrong nonce length": func(s secrets.Sealed) secrets.Sealed {
			s.Nonce = s.Nonce[:len(s.Nonce)-1]
			return s
		},
	}

	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := sealer.Open(mutate(sealed), binding); !errors.Is(err, secrets.ErrCorrupt) {
				t.Errorf("Open = %v, want ErrCorrupt", err)
			}
		})
	}
}

// GCM's security collapses entirely if a nonce is reused with the same key, so
// every seal must draw a fresh one.
func TestEverySealUsesAFreshNonce(t *testing.T) {
	sealer := sealerFor(t)
	binding := aad("claude-code", "oauth_token")

	seen := make(map[string]struct{}, 64)
	for range 64 {
		sealed, err := sealer.Seal([]byte("identical plaintext"), binding)
		if err != nil {
			t.Fatalf("Seal: %v", err)
		}
		key := base64.StdEncoding.EncodeToString(sealed.Nonce)
		if _, dup := seen[key]; dup {
			t.Fatal("a nonce was reused")
		}
		seen[key] = struct{}{}
	}
}

// Sealing the same plaintext twice must not produce the same ciphertext, or an
// observer could tell that two providers share a credential.
func TestSealingIsNotDeterministic(t *testing.T) {
	sealer := sealerFor(t)
	binding := aad("claude-code", "oauth_token")

	first, err := sealer.Seal([]byte("same"), binding)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	second, err := sealer.Seal([]byte("same"), binding)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	if bytes.Equal(first.Ciphertext, second.Ciphertext) {
		t.Error("identical plaintexts produced identical ciphertext")
	}
}

// A different key must not open it — and the error should say the custody
// changed, because that is the recoverable case (ADR-0002): a database restored
// to a new host needs its credentials submitted again.
func TestAnotherKeyCannotOpenIt(t *testing.T) {
	first := sealerFor(t)
	second := sealerFor(t)

	binding := aad("claude-code", "oauth_token")
	sealed, err := first.Seal([]byte("a-credential"), binding)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	_, err = second.Open(sealed, binding)
	if !errors.Is(err, secrets.ErrCorrupt) {
		t.Fatalf("Open with a foreign key = %v, want ErrCorrupt", err)
	}
	if !strings.Contains(err.Error(), "submitted again") {
		t.Errorf("the error should explain the recovery, got: %v", err)
	}
}

func TestEmptyPlaintextRoundTrips(t *testing.T) {
	sealer := sealerFor(t)
	binding := aad("claude-code", "oauth_token")

	sealed, err := sealer.Seal(nil, binding)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	opened, err := sealer.Open(sealed, binding)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if len(opened) != 0 {
		t.Errorf("opened %q, want empty", opened)
	}
}

func TestUnsupportedCipherIsRefused(t *testing.T) {
	sealer := sealerFor(t)
	binding := aad("claude-code", "oauth_token")

	sealed, err := sealer.Seal([]byte("x"), binding)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	sealed.Cipher = "rot13"

	if _, err := sealer.Open(sealed, binding); err == nil {
		t.Error("Open accepted a row sealed with an unknown cipher")
	}
}

// --- key stores ----------------------------------------------------------

// The environment override wins over everything, or it would not be an
// override.
func TestEnvKeyStoreTakesPrecedence(t *testing.T) {
	key := bytes.Repeat([]byte{0x2a}, secrets.KeySize)
	t.Setenv(secrets.MasterKeyEnv, base64.StdEncoding.EncodeToString(key))

	keyFile := filepath.Join(t.TempDir(), "master.key")
	store, err := secrets.OpenKeyStore(keyFile)
	if err != nil {
		t.Fatalf("OpenKeyStore: %v", err)
	}

	if store.Backend() != secrets.BackendEnv {
		t.Errorf("Backend = %q, want %q", store.Backend(), secrets.BackendEnv)
	}
	if _, err := os.Stat(keyFile); !errors.Is(err, os.ErrNotExist) {
		t.Error("a key file was created despite the environment override")
	}

	got, err := store.Key()
	if err != nil {
		t.Fatalf("Key: %v", err)
	}
	if !bytes.Equal(got, key) {
		t.Error("the key does not match the environment value")
	}
}

func TestEnvKeyStoreRejectsAnUnusableValue(t *testing.T) {
	for _, value := range []string{"too-short", base64.StdEncoding.EncodeToString([]byte("16-bytes-exactly"))} {
		if _, err := secrets.NewEnvKeyStore(value); err == nil {
			t.Errorf("NewEnvKeyStore accepted %q as a master key", value)
		}
	}
}

func TestFileKeyStoreCreatesAnOwnerOnlyKey(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "nested", "master.key")

	store, err := secrets.NewFileKeyStore(keyFile)
	if err != nil {
		t.Fatalf("NewFileKeyStore: %v", err)
	}
	if store.Backend() != secrets.BackendFile {
		t.Errorf("Backend = %q, want %q", store.Backend(), secrets.BackendFile)
	}

	info, err := os.Stat(keyFile)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("key file mode = %#o, want 0600", perm)
	}

	// Re-opening must reuse the key, not mint a new one — otherwise every
	// restart would orphan every stored credential.
	again, err := secrets.NewFileKeyStore(keyFile)
	if err != nil {
		t.Fatalf("re-open: %v", err)
	}
	first, _ := store.Key()
	second, _ := again.Key()
	if !bytes.Equal(first, second) {
		t.Error("re-opening the key file produced a different key")
	}
}

// A key file others can read is not a key file. Refusing is right, and refusing
// rather than tightening is right too: unlike a directory tumika creates, a
// loose mode here means the key may already have been read.
func TestFileKeyStoreRefusesAReadableKey(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "master.key")

	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x11}, secrets.KeySize))
	if err := os.WriteFile(keyFile, []byte(key), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, err := secrets.NewFileKeyStore(keyFile)
	if err == nil {
		t.Fatal("NewFileKeyStore accepted a world-readable key file")
	}
	if !strings.Contains(err.Error(), "submit every credential again") {
		t.Errorf("the error should explain the consequence, got: %v", err)
	}
}
