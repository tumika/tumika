//go:build darwin

package tokencustody

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// Every test here injects its own setter. Nothing in this file may reach
// keyring.Set: `go test ./...` must not write into the Keychain of whoever ran
// it.

func TestKeychainStorePassesIdentifiersAndToken(t *testing.T) {
	var gotService, gotAccount, gotSecret string
	c := &keychainCustodian{set: func(service, account, secret string) error {
		gotService, gotAccount, gotSecret = service, account, secret
		return nil
	}}

	if err := c.Store(context.Background(), "tk_secret"); err != nil {
		t.Fatalf("Store: %v", err)
	}
	if gotService != "tumika" {
		t.Errorf("service = %q, want %q", gotService, "tumika")
	}
	if gotAccount != "api-token" {
		t.Errorf("account = %q, want %q", gotAccount, "api-token")
	}
	if gotSecret != "tk_secret" {
		t.Errorf("secret = %q, want the token", gotSecret)
	}
}

func TestKeychainStoreWrapsTheSetterErrorWithoutTheToken(t *testing.T) {
	sentinel := errors.New("keychain is locked")
	c := &keychainCustodian{set: func(string, string, string) error { return sentinel }}

	err := c.Store(context.Background(), "tk_secret")
	if !errors.Is(err, sentinel) {
		t.Fatalf("Store error = %v, want it to wrap %v", err, sentinel)
	}
	if strings.Contains(err.Error(), "tk_secret") {
		t.Errorf("the token leaked into the error: %q", err)
	}
}

func TestKeychainStoreRefusesACancelledContext(t *testing.T) {
	called := false
	c := &keychainCustodian{set: func(string, string, string) error {
		called = true
		return nil
	}}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := c.Store(ctx, "tk_secret"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Store error = %v, want context.Canceled", err)
	}
	if called {
		t.Error("the setter ran for a cancelled context")
	}
}

func TestNewInjectsTheRealKeychainSetter(t *testing.T) {
	c, ok := New().(*keychainCustodian)
	if !ok {
		t.Fatalf("New returned %T, want *keychainCustodian", New())
	}
	if c.set == nil {
		t.Error("New left the setter nil")
	}
}
