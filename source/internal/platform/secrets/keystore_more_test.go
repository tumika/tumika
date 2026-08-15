package secrets_test

import (
	"bytes"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/tumika/tumika/source/internal/platform/secrets"
)

// stubStore lets the Sealer's own error handling be tested without a real
// backend.
type stubStore struct {
	key     []byte
	err     error
	backend string
	ref     string
}

func (s stubStore) Key() ([]byte, error) { return s.key, s.err }
func (s stubStore) Backend() string      { return s.backend }
func (s stubStore) KeyRef() string       { return s.ref }

func TestNewRejectsAnUnusableKey(t *testing.T) {
	tests := map[string]stubStore{
		"too short": {key: bytes.Repeat([]byte{1}, 16), backend: "stub"},
		"too long":  {key: bytes.Repeat([]byte{1}, 64), backend: "stub"},
		"empty":     {key: nil, backend: "stub"},
		"store failed": {
			err:     errors.New("keychain said no"),
			backend: "stub",
		},
	}

	for name, store := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := secrets.New(store); err == nil {
				t.Error("New accepted a key it should have rejected")
			}
		})
	}
}

func TestSealerReportsItsBackendAndKeyRef(t *testing.T) {
	store := stubStore{
		key:     bytes.Repeat([]byte{7}, secrets.KeySize),
		backend: "stub",
		ref:     "stub:somewhere",
	}

	sealer, err := secrets.New(store)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if sealer.Backend() != "stub" {
		t.Errorf("Backend = %q", sealer.Backend())
	}
	if sealer.KeyRef() != "stub:somewhere" {
		t.Errorf("KeyRef = %q", sealer.KeyRef())
	}

	sealed, err := sealer.Seal([]byte("x"), nil)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	// The ref is stamped on the row, which is what makes re-keying a query
	// rather than an excavation.
	if sealed.KeyRef != "stub:somewhere" {
		t.Errorf("sealed.KeyRef = %q", sealed.KeyRef)
	}
}

func TestKeyEncodingsAccepted(t *testing.T) {
	key := bytes.Repeat([]byte{0x5c}, secrets.KeySize)

	// An operator should not have to think about which base64 flavour to use,
	// nor be forced to encode at all.
	for name, encoded := range map[string]string{
		"standard":          base64.StdEncoding.EncodeToString(key),
		"raw standard":      base64.RawStdEncoding.EncodeToString(key),
		"url":               base64.URLEncoding.EncodeToString(key),
		"raw url":           base64.RawURLEncoding.EncodeToString(key),
		"surrounding space": "  " + base64.StdEncoding.EncodeToString(key) + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			store, err := secrets.NewEnvKeyStore(encoded)
			if err != nil {
				t.Fatalf("NewEnvKeyStore: %v", err)
			}
			got, err := store.Key()
			if err != nil {
				t.Fatalf("Key: %v", err)
			}
			if !bytes.Equal(got, key) {
				t.Error("the decoded key does not match")
			}
			if store.KeyRef() == "" {
				t.Error("KeyRef must not be empty")
			}
		})
	}
}

// A truncated key must be an error, not a different key.
//
// The first 32 characters of a 44-character base64 key decode to 24 bytes. An
// earlier version rejected that on length and then accepted the 32 characters
// verbatim as raw key material — so a value truncated in a systemd unit or a
// .env started the daemon under a key nobody intended, and every stored
// credential failed to open with no indication why.
func TestTruncatedKeyIsRejectedRatherThanReinterpreted(t *testing.T) {
	full := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x3b}, secrets.KeySize))

	for _, truncated := range []string{full[:32], full[:16], full[:40]} {
		store, err := secrets.NewEnvKeyStore(truncated)
		if err == nil {
			got, _ := store.Key()
			t.Errorf("NewEnvKeyStore(%q) was accepted as a %d-byte key", truncated, len(got))
			continue
		}
		if !errors.Is(err, secrets.ErrKeyLength) {
			t.Errorf("NewEnvKeyStore(%q) = %v, want ErrKeyLength", truncated, err)
		}
	}
}

func TestEnvKeyStoreRejectsEmpty(t *testing.T) {
	if _, err := secrets.NewEnvKeyStore(""); !errors.Is(err, secrets.ErrNoKey) {
		t.Errorf("NewEnvKeyStore(\"\") = %v, want ErrNoKey", err)
	}
}

// A key file that exists but holds nonsense is a configuration error, not a
// reason to mint a replacement — minting one would silently orphan every stored
// credential.
func TestFileKeyStoreRefusesAnUnusableKeyFile(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "master.key")
	if err := os.WriteFile(keyFile, []byte("not-a-key"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	if _, err := secrets.NewFileKeyStore(keyFile); err == nil {
		t.Error("NewFileKeyStore accepted a key file it could not decode")
	}
}

func TestFileKeyStoreReportsAnUncreatablePath(t *testing.T) {
	blocked := filepath.Join(t.TempDir(), "in-the-way")
	if err := os.WriteFile(blocked, []byte("a file, not a directory"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	if _, err := secrets.NewFileKeyStore(filepath.Join(blocked, "master.key")); err == nil {
		t.Error("NewFileKeyStore succeeded with a file where its directory should be")
	}
}

// KeyRef is advisory, not a gate.
//
// A row whose ref no longer matches — the home directory moved, so the file
// store's path changed — must still open when the KEY is the same. Refusing on a
// ref mismatch would turn a relocated install into an unrecoverable one, which
// is the opposite of what the ref is for: it exists to explain a failure, not to
// cause one.
func TestKeyRefMismatchDoesNotBlockAValidKey(t *testing.T) {
	sealer := sealerFor(t)
	binding := aad("claude-code", "oauth_token")

	sealed, err := sealer.Seal([]byte("a-credential"), binding)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	sealed.KeyRef = "file:/somewhere/else/master.key"

	opened, err := sealer.Open(sealed, binding)
	if err != nil {
		t.Fatalf("a row with a stale KeyRef but the right key must still open: %v", err)
	}
	if string(opened) != "a-credential" {
		t.Errorf("opened %q", opened)
	}
}
