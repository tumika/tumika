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
)

// TokenPrefix marks tumika's own API token.
//
// It exists so the token is recognisable by shape, which is what lets the log
// redactor catch one that reaches a log line by accident — the same protection
// the Anthropic prefixes get. See
// .agents/rules/never-log-or-return-a-credential-secret.md.
const TokenPrefix = "tmk_"

// tokenBytes is the entropy behind the token. 32 bytes is well past anything
// brute-forceable and keeps the encoded form a manageable length.
const tokenBytes = 32

// ErrNoToken is returned when no API token has been configured. The daemon
// refuses to serve in that state rather than listening without authentication.
var ErrNoToken = errors.New("no API token configured")

// AuthService mints and verifies the API bearer token.
//
// Only the SHA-256 of the token is ever stored. The plaintext exists exactly
// once, in the output of `tumika token rotate`, and is unrecoverable
// afterwards — losing it means minting a new one, which is the correct
// trade for a credential that grants full API access.
type AuthService interface {
	// Rotate mints a new token, replacing any existing one, and returns the
	// plaintext. This is the only time the plaintext exists.
	Rotate(ctx context.Context) (string, error)
	// Configured reports whether a token has been set.
	Configured(ctx context.Context) (bool, error)
	// Verify reports whether presented matches the stored token.
	Verify(ctx context.Context, presented string) (bool, error)
}

type authService struct{ cfg ConfigService }

// NewAuthService builds the service.
//
// It depends on ConfigService rather than on a repository, because the settings
// store is owned by ConfigService and a second writer would bypass the rules
// that live there. Same shape as LoginService reaching credentials through
// ProviderService — see
// .agents/rules/a-repository-has-exactly-one-owning-service.md.
func NewAuthService(cfg ConfigService) AuthService { return &authService{cfg: cfg} }

func (s *authService) Rotate(ctx context.Context) (string, error) {
	raw := make([]byte, tokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate API token: %w", err)
	}

	// URL-safe and unpadded, so the token survives being pasted into a shell, a
	// URL or a YAML file without quoting or escaping.
	token := TokenPrefix + base64.RawURLEncoding.EncodeToString(raw)

	if err := s.cfg.WriteSecret(ctx, KeyAPITokenHash, hashToken(token)); err != nil {
		return "", err
	}
	return token, nil
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
