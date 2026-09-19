package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/tumika/tumika/source/daemon/internal/platform/filelock"
	"github.com/tumika/tumika/source/daemon/internal/platform/tokencustody"
)

// TokenPrefix marks tumika's own API token.
//
// It exists so the token is recognisable by shape, which is what lets the log
// redactor catch one that reaches a log line by accident — the same protection
// the Anthropic prefixes get. See
// agentic/rules/never-log-or-return-a-credential-secret.md.
const TokenPrefix = "tmk_"

// tokenBytes is the entropy behind the token. 32 bytes is well past anything
// brute-forceable and keeps the encoded form a manageable length.
const tokenBytes = 32

// ErrNoToken is returned when no API token has been configured. The daemon
// refuses to serve in that state rather than listening without authentication.
var ErrNoToken = errors.New("no API token configured")

// RotateResult is everything a rotation produces.
//
// The token and the custody outcome travel together because a caller has to act
// on both: print the plaintext, then say whether tumika also managed to hand it
// to the platform's secret store.
type RotateResult struct {
	// Token is the plaintext. The daemon returns it only from Rotate.
	Token string
	// CustodyErr reports that handing the token to the platform's secret store
	// failed. It never carries the token itself, and it is never fatal — the
	// new token is already the live one by the time custody is attempted.
	CustodyErr error
}

// AuthService mints and verifies the API bearer token.
//
// Only the SHA-256 of the token is stored in the database, so the daemon can
// never recover a token. The plaintext is shown once when minted and, on macOS,
// also handed to the login Keychain; losing both means minting a new one, which
// is the correct trade for a credential that grants full API access.
type AuthService interface {
	// Rotate mints a new token, replacing any existing one, and returns the
	// plaintext together with the outcome of handing it to the platform's
	// secret store. A custody failure is reported in the result, not as an error.
	// Rotations are serialised against each other, including across processes.
	Rotate(ctx context.Context) (RotateResult, error)
	// Configured reports whether a token has been set.
	Configured(ctx context.Context) (bool, error)
	// Verify reports whether presented matches the stored token.
	Verify(ctx context.Context, presented string) (bool, error)
}

type authService struct {
	cfg     ConfigService
	custody tokencustody.Custodian
	locker  filelock.Locker
}

// NewAuthService builds the service.
//
// It depends on ConfigService rather than on a repository, because the settings
// store is owned by ConfigService and a second writer would bypass the rules
// that live there. Same shape as LoginService reaching credentials through
// ProviderService — see
// agentic/rules/a-repository-has-exactly-one-owning-service.md.
//
// The Custodian is the platform's secret store. It is injected rather than
// selected here so a test never reaches a real keystore — see
// platform/tokencustody.
//
// The Locker serialises rotations; see Rotate for what it guards.
func NewAuthService(cfg ConfigService, custody tokencustody.Custodian, locker filelock.Locker) AuthService {
	return &authService{cfg: cfg, custody: custody, locker: locker}
}

func (s *authService) Rotate(ctx context.Context) (RotateResult, error) {
	// One rotation at a time, across processes. `tumika token rotate` runs in
	// its own process, and a rotation is two writes — the hash, then custody.
	// Interleaved, two of them leave the hash from one rotation and the
	// Keychain copy from the other: both report success, and the token the
	// operator can retrieve is one the daemon rejects.
	//
	// The lock is taken before anything is minted, so a caller that cannot
	// take it has changed nothing.
	unlock, err := s.locker.Lock(ctx)
	if err != nil {
		return RotateResult{}, fmt.Errorf("serialize token rotation: %w", err)
	}
	defer unlock()

	raw := make([]byte, tokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return RotateResult{}, fmt.Errorf("generate API token: %w", err)
	}

	// URL-safe and unpadded, so the token survives being pasted into a shell, a
	// URL or a YAML file without quoting or escaping.
	token := TokenPrefix + base64.RawURLEncoding.EncodeToString(raw)

	// The hash is written before custody is attempted, so the store never holds
	// a token the daemon does not accept. The reverse order puts an operator in
	// front of a keystore entry that authenticates against nothing.
	if err := s.cfg.WriteSecret(ctx, KeyAPITokenHash, hashToken(token)); err != nil {
		return RotateResult{}, err
	}

	// A custody failure rides in the result, never in the error. Returning it as
	// the error would discard the plaintext for a hash that is already stored —
	// a daemon holding a token nobody has ever seen, which is the one state
	// rotation exists to avoid.
	return RotateResult{Token: token, CustodyErr: s.custody.Store(ctx, token)}, nil
}

func (s *authService) Configured(ctx context.Context) (bool, error) {
	_, err := s.cfg.ReadSecret(ctx, KeyAPITokenHash)
	switch {
	case errors.Is(err, ErrNotSet):
		return false, nil
	case err != nil:
		return false, err
	}
	return true, nil
}

// Verify compares the presented token against the stored hash in constant time.
//
// Both the hash comparison and the not-configured path do the same amount of
// work: an early return on "no token stored" would make the daemon's
// configuration state measurable by an unauthenticated caller.
func (s *authService) Verify(ctx context.Context, presented string) (bool, error) {
	stored, err := s.cfg.ReadSecret(ctx, KeyAPITokenHash)
	if err != nil && !errors.Is(err, ErrNotSet) {
		return false, err
	}

	// A hash of the right length that cannot match anything, so the comparison
	// below runs identically whether or not a token is configured.
	if errors.Is(err, ErrNotSet) {
		stored = hex.EncodeToString(make([]byte, sha256.Size))
	}

	match := subtle.ConstantTimeCompare([]byte(hashToken(presented)), []byte(stored)) == 1
	return match && !errors.Is(err, ErrNotSet), nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
