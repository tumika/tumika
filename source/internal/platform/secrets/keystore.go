package secrets

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// MasterKeyEnv overrides key custody entirely. Intended for containers and for
// operators who bring their own key management (ADR-0002).
const MasterKeyEnv = "TUMIKA_MASTER_KEY"

// Backend names, as reported by /v1/health.
const (
	BackendEnv      = "env"
	BackendKeychain = "keychain"
	BackendFile     = "file"
)

// KeyStore holds the master key. It is the only part of sealing that varies by
// platform; the cipher does not.
type KeyStore interface {
	// Key returns the master key, creating one if this store is allowed to and
	// none exists.
	Key() ([]byte, error)
	// Backend names the custody, for /v1/health.
	Backend() string
	// KeyRef identifies WHICH key, so a sealed row can say what opened it.
	KeyRef() string
}

// ErrNoKey is returned by a store that has no key and cannot create one.
var ErrNoKey = errors.New("no master key available")

// OpenKeyStore chooses key custody for this machine.
//
// Callers that need a SPECIFIC backend — tests, and a future re-key command —
// construct one directly with NewFileKeyStore or NewEnvKeyStore. Selection and
// construction are separate on purpose: a test calling OpenKeyStore on a Mac
// would reach for the real login Keychain and write a key into it, which is a
// side effect no test should have on the machine running it.
//
// Precedence is explicit-over-implicit: an operator who set the environment
// variable meant it, and silently preferring a keychain entry over it would make
// the override untrustworthy. After that the platform's own facility is
// preferred, and the file is the fallback — honest about being one.
//
// The Linux systemd-creds backend joins at the service-manager step; until then
// Linux gets the file, which is what a container would use anyway.
func OpenKeyStore(keyFile string) (KeyStore, error) {
	if raw := os.Getenv(MasterKeyEnv); raw != "" {
		return newEnvKeyStore(raw)
	}

	if runtime.GOOS == "darwin" {
		store, err := newKeychainKeyStore()
		if err == nil {
			return store, nil
		}
		// A Mac without a usable Keychain is a real configuration — a daemon
		// with no user session, most obviously. Falling back to the file keeps
		// it working; the backend is reported in /v1/health either way, so the
		// choice is visible rather than silent.
		if !errors.Is(err, errKeychainUnavailable) {
			return nil, err
		}
	}

	return newFileKeyStore(keyFile)
}

// envKeyStore takes the key from the environment. It creates nothing: if the
// value is unusable that is a configuration error, and inventing a key would
// silently encrypt everything under something the operator does not have.
type envKeyStore struct{ key []byte }

// NewEnvKeyStore builds a key store from an encoded key, without consulting the
// environment.
func NewEnvKeyStore(encoded string) (KeyStore, error) { return newEnvKeyStore(encoded) }

func newEnvKeyStore(raw string) (KeyStore, error) {
	key, err := decodeKey(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", MasterKeyEnv, err)
	}
	return &envKeyStore{key: key}, nil
}

func (s *envKeyStore) Key() ([]byte, error) { return s.key, nil }
func (s *envKeyStore) Backend() string      { return BackendEnv }
func (s *envKeyStore) KeyRef() string       { return BackendEnv + ":" + MasterKeyEnv }

// fileKeyStore keeps the key in a 0600 file beside the database.
//
// The weakest backend, and honest about it: anything that can read the database
// can probably read this too. It exists because a container has no keystore, and
// TUMIKA_MASTER_KEY is the intended answer for anyone running containers
// seriously.
type fileKeyStore struct {
	path string
	key  []byte
}

// NewFileKeyStore builds a file-backed key store at path, minting a key if none
// exists. It never consults the Keychain or the environment.
func NewFileKeyStore(path string) (KeyStore, error) { return newFileKeyStore(path) }

func newFileKeyStore(path string) (KeyStore, error) {
	store := &fileKeyStore{path: path}

	key, err := store.load()
	switch {
	case err == nil:
		store.key = key
		return store, nil
	case !errors.Is(err, os.ErrNotExist):
		return nil, err
	}

	key, err = store.create()
	if err != nil {
		return nil, err
	}
	store.key = key
	return store, nil
}

func (s *fileKeyStore) load() ([]byte, error) {
	info, err := os.Stat(s.path)
	if err != nil {
		return nil, err
	}

	// A key file anyone can read is not a key file. Refuse rather than quietly
	// carrying on, and refuse rather than tightening: unlike a directory tumika
	// creates, a loose mode here means the key may already have been read.
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("%s has mode %#o and may already have been read by others; "+
			"delete it to mint a new key, and expect to submit every credential again",
			s.path, perm)
	}

	raw, err := os.ReadFile(s.path)
	if err != nil {
		return nil, err
	}
	return decodeKey(strings.TrimSpace(string(raw)))
}

func (s *fileKeyStore) create() ([]byte, error) {
	key := make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate master key: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return nil, fmt.Errorf("create %s: %w", filepath.Dir(s.path), err)
	}

	// O_EXCL so two daemons racing to first-start cannot both mint a key and
	// leave one of them holding credentials nothing can open.
	f, err := os.OpenFile(s.path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create %s: %w", s.path, err)
	}
	defer func() { _ = f.Close() }()

	if _, err := f.WriteString(base64.StdEncoding.EncodeToString(key)); err != nil {
		return nil, fmt.Errorf("write %s: %w", s.path, err)
	}
	return key, nil
}

func (s *fileKeyStore) Key() ([]byte, error) { return s.key, nil }
func (s *fileKeyStore) Backend() string      { return BackendFile }
func (s *fileKeyStore) KeyRef() string       { return BackendFile + ":" + s.path }

// decodeKey accepts base64 (standard or URL, padded or not) or a hex-length raw
// string, and insists on exactly KeySize bytes.
func decodeKey(s string) ([]byte, error) {
	if s == "" {
		return nil, ErrNoKey
	}

	for _, enc := range []*base64.Encoding{
		base64.StdEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.RawURLEncoding,
	} {
		if key, err := enc.DecodeString(s); err == nil && len(key) == KeySize {
			return key, nil
		}
	}

	// A raw 32-byte string is accepted too, so an operator can supply one
	// without having to think about encoding.
	if len(s) == KeySize {
		return []byte(s), nil
	}

	return nil, fmt.Errorf("%w: expected %d bytes, base64-encoded", ErrKeyLength, KeySize)
}
