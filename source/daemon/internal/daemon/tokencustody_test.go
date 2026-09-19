package daemon

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/tumika/tumika/source/daemon/internal/platform/paths"
	"github.com/tumika/tumika/source/daemon/internal/platform/secrets"
	"github.com/tumika/tumika/source/daemon/internal/platform/tokencustody"
)

// recordingCustodian stands in for a platform secret store.
type recordingCustodian struct {
	stored []string
}

func (c *recordingCustodian) Store(_ context.Context, token string) error {
	c.stored = append(c.stored, token)
	return nil
}

func TestTokenCustodyDefaultsToStoringNothing(t *testing.T) {
	// The failure this guards is silent and off-machine: on macOS the real
	// custodian writes the token into the login Keychain of whoever ran the
	// tests, and every test that builds a daemon leaves TokenCustody unset.
	if got := resolveTokenCustody(Options{}); got != tokencustody.NewNoop() {
		t.Errorf("resolveTokenCustody(Options{}) = %#v, want the no-op custodian", got)
	}
}

func TestTokenCustodyFromOptionsIsUsed(t *testing.T) {
	custody := &recordingCustodian{}

	if got := resolveTokenCustody(Options{TokenCustody: custody}); got != custody {
		t.Fatalf("resolveTokenCustody = %#v, want the injected custodian", got)
	}
}

// TestRotateHandsTheTokenToTheInjectedCustodian wires a real daemon, so it
// proves the option reaches AuthService rather than only reaching New.
func TestRotateHandsTheTokenToTheInjectedCustodian(t *testing.T) {
	t.Setenv(secrets.MasterKeyEnv, "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")

	p, err := paths.Resolve(filepath.Join(t.TempDir(), "home"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	custody := &recordingCustodian{}
	ctx := context.Background()
	d, err := New(ctx, Options{Paths: p, TokenCustody: custody})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if closeErr := d.Close(); closeErr != nil {
			t.Errorf("Close: %v", closeErr)
		}
	})

	res, err := d.AuthService().Rotate(ctx)
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if res.CustodyErr != nil {
		t.Fatalf("CustodyErr = %v, want nil", res.CustodyErr)
	}
	if len(custody.stored) != 1 || custody.stored[0] != res.Token {
		t.Errorf("custodian received %d token(s), want the rotated one", len(custody.stored))
	}
}
