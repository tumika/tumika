package service_test

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/tumika/tumika/source/internal/service"
)

func newAuth(t *testing.T) (service.AuthService, service.ConfigService) {
	t.Helper()
	cfg, _, _ := newService(t)
	return service.NewAuthService(cfg), cfg
}

func TestRotateMintsAVerifiableToken(t *testing.T) {
	auth, _ := newAuth(t)
	ctx := t.Context()

	configured, err := auth.Configured(ctx)
	if err != nil {
		t.Fatalf("Configured: %v", err)
	}
	if configured {
		t.Fatal("a fresh install must have no token")
	}

	token, err := auth.Rotate(ctx)
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}

	if !strings.HasPrefix(token, service.TokenPrefix) {
		t.Errorf("token %q lacks the %q prefix that makes it redactable by shape", token, service.TokenPrefix)
	}
	if len(token) < 40 {
		t.Errorf("token is only %d characters; that is not 32 bytes of entropy", len(token))
	}

	ok, err := auth.Verify(ctx, token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !ok {
		t.Error("the minted token does not verify")
	}
}

// Only the hash is stored. A database that leaks must not yield a working
// credential.
func TestOnlyTheHashIsStored(t *testing.T) {
	auth, cfg := newAuth(t)
	ctx := t.Context()

	token, err := auth.Rotate(ctx)
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}

	stored, err := cfg.ReadSecret(ctx, service.KeyAPITokenHash)
	if err != nil {
		t.Fatalf("ReadSecret: %v", err)
	}
	if strings.Contains(stored, token) || stored == token {
		t.Fatal("the plaintext token was stored")
	}

	sum := sha256.Sum256([]byte(token))
	if stored != hex.EncodeToString(sum[:]) {
		t.Errorf("stored value is not the token's SHA-256")
	}
}

func TestVerifyRejectsEverythingElse(t *testing.T) {
	auth, _ := newAuth(t)
	ctx := t.Context()

	token, err := auth.Rotate(ctx)
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}

	for _, presented := range []string{
		"",
		"wrong",
		token + "x",
		token[:len(token)-1],
		strings.ToUpper(token),
	} {
		ok, err := auth.Verify(ctx, presented)
		if err != nil {
			t.Fatalf("Verify(%q): %v", presented, err)
		}
		if ok {
			t.Errorf("Verify(%q) accepted a token it should not have", presented)
		}
	}
}

// Rotating invalidates the previous token immediately; there is no grace period,
// because a rotation is usually a response to a suspected leak.
func TestRotateInvalidatesThePreviousToken(t *testing.T) {
	auth, _ := newAuth(t)
	ctx := t.Context()

	first, err := auth.Rotate(ctx)
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	second, err := auth.Rotate(ctx)
	if err != nil {
		t.Fatalf("second Rotate: %v", err)
	}
	if first == second {
		t.Fatal("Rotate returned the same token twice")
	}

	if ok, _ := auth.Verify(ctx, first); ok {
		t.Error("the previous token still verifies after a rotation")
	}
	if ok, _ := auth.Verify(ctx, second); !ok {
		t.Error("the new token does not verify")
	}
}

// With no token configured, Verify must answer "no" rather than "yes" — and must
// not take a visibly different path, since that would let an unauthenticated
// caller measure whether the daemon is configured.
func TestVerifyWithNoTokenConfigured(t *testing.T) {
	auth, _ := newAuth(t)

	ok, err := auth.Verify(t.Context(), "tmk_anything")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if ok {
		t.Fatal("Verify accepted a token when none is configured")
	}

	// The empty string is the degenerate case: an implementation comparing
	// against an unset value could accept it.
	if ok, _ := auth.Verify(t.Context(), ""); ok {
		t.Fatal("Verify accepted an empty token when none is configured")
	}
}

func TestTokensAreUnique(t *testing.T) {
	auth, _ := newAuth(t)
	seen := make(map[string]struct{}, 32)

	for range 32 {
		token, err := auth.Rotate(t.Context())
		if err != nil {
			t.Fatalf("Rotate: %v", err)
		}
		if _, dup := seen[token]; dup {
			t.Fatal("Rotate produced a duplicate token")
		}
		seen[token] = struct{}{}
	}
}
