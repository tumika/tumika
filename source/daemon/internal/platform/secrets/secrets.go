// Package secrets seals credentials for storage.
//
// tumika uses envelope encryption (ADR-0002): AES-256-GCM ciphertext lives in
// SQLite alongside everything else, and only the KEY leaves, into whatever
// custody the platform provides. That is what keeps "the database is the whole
// state of the system" true — the file remains complete and backup-able, it is
// simply not readable on its own.
//
// The cipher is fixed and the key custody is pluggable, not the other way round.
// Choosing between backends is a deployment question; choosing between ciphers
// is not a question anyone should be asked.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
)

// CipherAESGCM names the only cipher tumika uses. It is recorded on every sealed
// row so a future change is a migration rather than an archaeology exercise.
const CipherAESGCM = "aes-256-gcm"

// KeySize is the master key length: AES-256.
const KeySize = 32

var (
	// ErrCorrupt is returned when ciphertext fails authentication. It means the
	// data was altered, the AAD does not match, or the key is wrong — GCM
	// cannot tell those apart, and pretending otherwise would be a lie.
	ErrCorrupt = errors.New("sealed data failed authentication")

	// ErrKeyLength is returned when a supplied key is not KeySize bytes.
	ErrKeyLength = fmt.Errorf("master key must be %d bytes", KeySize)
)

// Sealed is a credential at rest: ciphertext plus what is needed to open it
// again. It carries no plaintext and is safe to store, back up and log the
// existence of.
type Sealed struct {
	Ciphertext []byte
	Nonce      []byte
	// KeyRef records WHICH key custody sealed this, so re-keying after a
	// platform change is a query rather than a guess, and so a failure to open
	// can say why (ADR-0002).
	KeyRef string
	Cipher string
}

// Sealer seals and opens credential material.
type Sealer interface {
	// Seal encrypts plaintext, binding it to aad.
	Seal(plaintext, aad []byte) (Sealed, error)
	// Open decrypts, verifying the same aad. It returns ErrCorrupt if the data,
	// the AAD or the key does not match.
	Open(s Sealed, aad []byte) ([]byte, error)
	// Backend names the key custody in use, surfaced in /v1/health.
	Backend() string
	// KeyRef is what Seal will stamp on new rows.
	KeyRef() string
}

// aesSealer is AES-256-GCM over a key held by a KeyStore.
type aesSealer struct {
	aead    cipher.AEAD
	backend string
	keyRef  string
}

// New builds a Sealer from a key store, fetching the key once.
//
// Fetching once is deliberate: on macOS the key lives in the Keychain, and
// reading it per operation would mean a `security` invocation on every
// credential read — slow, and noisy in a way that trains an operator to click
// through prompts.
func New(store KeyStore) (Sealer, error) {
	key, err := store.Key()
	if err != nil {
		return nil, err
	}
	if len(key) != KeySize {
		return nil, fmt.Errorf("%w: %s returned %d", ErrKeyLength, store.Backend(), len(key))
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("initialise cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("initialise GCM: %w", err)
	}

	return &aesSealer{aead: aead, backend: store.Backend(), keyRef: store.KeyRef()}, nil
}

func (s *aesSealer) Backend() string { return s.backend }
func (s *aesSealer) KeyRef() string  { return s.keyRef }

func (s *aesSealer) Seal(plaintext, aad []byte) (Sealed, error) {
	// A fresh random nonce per seal. GCM's security collapses entirely if a
	// nonce is ever reused with the same key — not degrades, collapses — so
	// this is never derived from a counter or from the plaintext.
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return Sealed{}, fmt.Errorf("generate nonce: %w", err)
	}

	return Sealed{
		Ciphertext: s.aead.Seal(nil, nonce, plaintext, aad),
		Nonce:      nonce,
		KeyRef:     s.keyRef,
		Cipher:     CipherAESGCM,
	}, nil
}

func (s *aesSealer) Open(sealed Sealed, aad []byte) ([]byte, error) {
	if sealed.Cipher != "" && sealed.Cipher != CipherAESGCM {
		return nil, fmt.Errorf("sealed with unsupported cipher %q", sealed.Cipher)
	}
	if len(sealed.Nonce) != s.aead.NonceSize() {
		return nil, fmt.Errorf("%w: nonce is %d bytes, want %d",
			ErrCorrupt, len(sealed.Nonce), s.aead.NonceSize())
	}

	plaintext, err := s.aead.Open(nil, sealed.Nonce, sealed.Ciphertext, aad)
	if err != nil {
		// GCM reports one failure for a wrong key, altered ciphertext and
		// mismatched AAD alike. When the row was sealed by a DIFFERENT custody
		// than the one now in use, that is overwhelmingly the reason — and it is
		// the recoverable one, so say so rather than leaving an operator staring
		// at "authentication failed".
		if sealed.KeyRef != "" && sealed.KeyRef != s.keyRef {
			return nil, fmt.Errorf("%w: sealed by %q but the current key custody is %q; "+
				"the credential must be submitted again",
				ErrCorrupt, sealed.KeyRef, s.keyRef)
		}
		return nil, ErrCorrupt
	}
	return plaintext, nil
}
