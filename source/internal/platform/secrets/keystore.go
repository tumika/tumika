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
// the override untrustworthy.
//
// On macOS, an unreachable Keychain is a HARD FAILURE — there is deliberately no
// fallback. An earlier version fell through to the file store, which is a data
// loss bug rather than resilience: a locked Keychain, or a denied access prompt,
// would send a daemon that had been running for weeks to a file store holding no
// key, which would mint a fresh one. The daemon would start cleanly, report
// backend "file", and be unable to open a single existing credential — and any
// credential re-submitted during that run would be sealed under the new key,
// while the next successful start preferred the Keychain again. The two sets
// orphan each other, flip-flopping run to run.
//
// Failing closed costs one confusing startup. Falling back costs the
// credentials. ADR-0002 already handles the no-session case: macOS runs as a
// LaunchAgent precisely so a session exists, and TUMIKA_MASTER_KEY is the answer
// where it does not.
//
// The Linux systemd-creds backend joins at the service-manager step; until then
// Linux gets the file, which is what a container would use anyway.
func OpenKeyStore(keyFile string) (KeyStore, error) {
	if raw := os.Getenv(MasterKeyEnv); raw != "" {
		return newEnvKeyStore(raw)
	}

	if runtime.GOOS == "darwin" {
		return newKeychainKeyStore()
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
	if errors.Is(err, os.ErrExist) {
		// Another process created the key between our load and our create — a
		// supervisor restarting the daemon mid-first-run, or a CLI command and
		// the daemon initialising custody together. The winner's key is the
		// key; refusing to start would report a race as corruption.
		key, err = store.load()
	}
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

	// An empty file is treated as absent rather than as a broken key. It is what
	// a crash between create and write used to leave behind, and reporting "no
	// master key available" for it wedged startup with no hint that deleting the
	// file was the fix.
	if info.Size() == 0 {
		return nil, os.ErrNotExist
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

	// Written to a temp file, fsynced, then linked into place with O_EXCL
	// semantics. The key file therefore either does not exist or is complete:
	// writing in place left a window where a crash produced an empty file that
	// no later start could interpret.
	//
	// The link is what makes this safe against a concurrent first-run — the
	// loser gets os.ErrExist and reloads the winner's key rather than failing.
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".master.key.*")
	if err != nil {
		return nil, fmt.Errorf("create a temporary key file in %s: %w", filepath.Dir(s.path), err)
	}
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}()

	if err := tmp.Chmod(0o600); err != nil {
		return nil, fmt.Errorf("set permissions on %s: %w", tmp.Name(), err)
	}
	if _, err := tmp.WriteString(base64.StdEncoding.EncodeToString(key)); err != nil {
		return nil, fmt.Errorf("write %s: %w", tmp.Name(), err)
	}
	if err := tmp.Sync(); err != nil {
		return nil, fmt.Errorf("flush %s: %w", tmp.Name(), err)
	}
	if err := tmp.Close(); err != nil {
		return nil, fmt.Errorf("close %s: %w", tmp.Name(), err)
	}

	// os.Link fails with EEXIST rather than replacing, which os.Rename would do
	// silently — and replacing the key file is precisely what must never happen.
	if err := os.Link(tmp.Name(), s.path); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("install %s: %w", s.path, err)
		}

		// Something is already there. A zero-length file is the debris of a
		// crash between create and write — not a key, and nothing will ever be
		// able to read it — so clear it and take the slot. Anything non-empty is
		// a real key from whoever won the race, and os.ErrExist tells the caller
		// to load it rather than clobber it.
		if info, statErr := os.Stat(s.path); statErr == nil && info.Size() == 0 {
			if rmErr := os.Remove(s.path); rmErr == nil {
				if linkErr := os.Link(tmp.Name(), s.path); linkErr != nil {
					return nil, fmt.Errorf("install %s: %w", s.path, linkErr)
				}
				return key, nil
			}
		}
		return nil, err
	}
	return key, nil
}

func (s *fileKeyStore) Key() ([]byte, error) { return s.key, nil }
func (s *fileKeyStore) Backend() string      { return BackendFile }
func (s *fileKeyStore) KeyRef() string       { return BackendFile + ":" + s.path }

// decodeKey accepts base64 in any of its four flavours and insists on exactly
// KeySize bytes.
//
// There is deliberately no "raw bytes" fallback. It looked like a kindness —
// supply 32 characters and skip the encoding question — but it turned a
// truncated key into a silently different one: the first 32 characters of a
// 44-character base64 key decode to 24 bytes, fail the length check, and were
// then accepted verbatim as raw material. The daemon would start, use a key
// nobody intended, and fail to open every stored credential. An error naming the
// problem is worth far more than saving an operator a base64 call.
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
		key, err := enc.DecodeString(s)
		if err != nil {
			continue
		}
		if len(key) != KeySize {
			return nil, fmt.Errorf("%w, but this decoded to %d (is the value truncated?)",
				ErrKeyLength, len(key))
		}
		return key, nil
	}

	return nil, fmt.Errorf("%w, base64-encoded; this is not valid base64", ErrKeyLength)
}
