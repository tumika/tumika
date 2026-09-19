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

// errKeychainUnavailable means the Keychain cannot be reached.
//
// This is a HARD failure on macOS: there is no fallback, because falling back
// would mint a fresh key and orphan every credential sealed under the Keychain
// (see OpenKeyStore). The message names the way out.
var errKeychainUnavailable = errors.New(
	"macOS Keychain is unavailable; tumika will not fall back to a file key, because that " +
		"would mint a new key and orphan every credential already sealed. Unlock the keychain, " +
		"or set " + MasterKeyEnv + " to supply the key explicitly")

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
		// A locked keychain, a denied access prompt, or no session at all. Any
		// of these may sit in front of an EXISTING key, and there is no way from
		// here to tell. Refusing to start is the only answer that cannot lose
		// credentials.
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
