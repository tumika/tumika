//go:build darwin

package secrets

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/zalando/go-keyring"
)

// Keychain identifiers. Changing either orphans every existing credential, so
// they are constants rather than anything derived.
const (
	keychainService = "tumika"
	keychainAccount = "master-key"
)

// errKeychainUnavailable means the Keychain cannot be reached at all — most
// often a daemon with no user session. The caller falls back to a file rather
// than failing, and /v1/health reports which one is in use.
var errKeychainUnavailable = errors.New("keychain is unavailable")

// keychainKeyStore keeps the master key in the macOS Keychain.
//
// This is why the macOS install is a LaunchAgent rather than a system daemon
// (ADR-0002): Keychain access needs a user session, and a LaunchDaemon does not
// have one.
type keychainKeyStore struct{ key []byte }

func newKeychainKeyStore() (KeyStore, error) {
	stored, err := keyring.Get(keychainService, keychainAccount)
	switch {
	case err == nil:
		key, decodeErr := decodeKey(stored)
		if decodeErr != nil {
			return nil, fmt.Errorf("keychain entry %s/%s is unusable: %w",
				keychainService, keychainAccount, decodeErr)
		}
		return &keychainKeyStore{key: key}, nil

	case errors.Is(err, keyring.ErrNotFound):
		// First run: mint one.

	default:
		// No session, no keychain, or access denied. Distinguished from a
		// genuine failure so the caller can fall back rather than refuse to
		// start.
		return nil, fmt.Errorf("%w: %w", errKeychainUnavailable, err)
	}

	key := make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate master key: %w", err)
	}

	if err := keyring.Set(keychainService, keychainAccount, base64.StdEncoding.EncodeToString(key)); err != nil {
		return nil, fmt.Errorf("%w: storing a new key: %w", errKeychainUnavailable, err)
	}
	return &keychainKeyStore{key: key}, nil
}

func (s *keychainKeyStore) Key() ([]byte, error) { return s.key, nil }
func (s *keychainKeyStore) Backend() string      { return BackendKeychain }

func (s *keychainKeyStore) KeyRef() string {
	return BackendKeychain + ":" + keychainService + "/" + keychainAccount
}
