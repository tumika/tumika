package service_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/tumika/tumika/source/internal/service"
)

// fakeCustodian stands in for the platform's secret store, so no test ever
// reaches a real keystore. It can refuse, which is the interesting case: a
// custody failure must not cost the caller the token.
type fakeCustodian struct {
	err    error
	calls  int
	stored string

	// cfg and hashAtStore record the stored hash as it stood when Store ran.
	// That is how the ordering is pinned: custody must not be offered a token
	// the daemon does not yet accept.
	cfg         service.ConfigService
	hashAtStore string
}

func (c *fakeCustodian) Store(ctx context.Context, token string) error {
	c.calls++
	c.stored = token
	if c.cfg != nil {
		c.hashAtStore, _ = c.cfg.ReadSecret(ctx, service.KeyAPITokenHash)
	}
	return c.err
}

func newAuth(t *testing.T) (service.AuthService, service.ConfigService) {
	t.Helper()
	auth, cfg, _ := newAuthWithCustody(t, &fakeCustodian{})
	return auth, cfg
}

func newAuthWithCustody(t *testing.T, custody *fakeCustodian) (service.AuthService, service.ConfigService, *fakeCustodian) {
	t.Helper()
	cfg, _, _ := newService(t)
	custody.cfg = cfg
	return service.NewAuthService(cfg, custody), cfg, custody
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

	minted, err := auth.Rotate(ctx)
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	token := minted.Token

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

	minted, err := auth.Rotate(ctx)
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	token := minted.Token

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

	minted, err := auth.Rotate(ctx)
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	token := minted.Token

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
	if first.Token == second.Token {
		t.Fatal("Rotate returned the same token twice")
	}

	if ok, _ := auth.Verify(ctx, first.Token); ok {
		t.Error("the previous token still verifies after a rotation")
	}
	if ok, _ := auth.Verify(ctx, second.Token); !ok {
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
		minted, err := auth.Rotate(t.Context())
		if err != nil {
			t.Fatalf("Rotate: %v", err)
		}
		token := minted.Token
		if _, dup := seen[token]; dup {
			t.Fatal("Rotate produced a duplicate token")
		}
		seen[token] = struct{}{}
	}
}

func TestRotateHandsTheTokenToCustody(t *testing.T) {
	auth, _, custody := newAuthWithCustody(t, &fakeCustodian{})

	minted, err := auth.Rotate(t.Context())
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if custody.calls != 1 {
		t.Fatalf("custody Store called %d times, want once", custody.calls)
	}
	if custody.stored != minted.Token {
		t.Error("custody was handed a different token than the caller was")
	}
	if minted.CustodyErr != nil {
		t.Errorf("CustodyErr = %v on a working custodian", minted.CustodyErr)
	}
}

// Custody is the backup copy, not the credential. A store that refuses must
// still leave the caller with a usable token to print, because the hash is
// already live by then and no second chance to read the plaintext exists.
func TestRotateSurvivesACustodyFailure(t *testing.T) {
	refused := errors.New("keystore is locked")
	auth, cfg, _ := newAuthWithCustody(t, &fakeCustodian{err: refused})
	ctx := t.Context()

	minted, err := auth.Rotate(ctx)
	if err != nil {
		t.Fatalf("Rotate returned an error for a custody failure: %v", err)
	}
	if minted.Token == "" {
		t.Fatal("Rotate returned no token after a custody failure")
	}
	if !errors.Is(minted.CustodyErr, refused) {
		t.Errorf("CustodyErr = %v, want the custodian's refusal", minted.CustodyErr)
	}

	stored, err := cfg.ReadSecret(ctx, service.KeyAPITokenHash)
	if err != nil {
		t.Fatalf("ReadSecret: %v", err)
	}
	sum := sha256.Sum256([]byte(minted.Token))
	if stored != hex.EncodeToString(sum[:]) {
		t.Error("the hash of the returned token is not the one stored")
	}

	if ok, _ := auth.Verify(ctx, minted.Token); !ok {
		t.Error("the token does not verify after a custody failure")
	}
}

// The hash lands before custody is offered the token. The other order puts a
// token in the operator's keystore that the daemon would reject.
func TestRotateStoresTheHashBeforeCallingCustody(t *testing.T) {
	auth, _, custody := newAuthWithCustody(t, &fakeCustodian{})

	minted, err := auth.Rotate(t.Context())
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}

	sum := sha256.Sum256([]byte(minted.Token))
	if custody.hashAtStore != hex.EncodeToString(sum[:]) {
		t.Errorf("hash at custody time = %q, want the new token's hash", custody.hashAtStore)
	}
}

// A custody error is reported, and reporting it must not be how the token
// escapes. Everything the caller can surface is checked, not just the message.
func TestCustodyErrorNeverCarriesTheToken(t *testing.T) {
	auth, _, _ := newAuthWithCustody(t, &fakeCustodian{err: errors.New("keystore is locked")})

	minted, err := auth.Rotate(t.Context())
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if strings.Contains(minted.CustodyErr.Error(), minted.Token) {
		t.Fatal("the custody error carries the token")
	}
}
