package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/tumika/tumika/source/internal/platform/tokencustody"
)

// recordingCustodian stands in for a platform secret store.
type recordingCustodian struct {
	stored []string
}

func (c *recordingCustodian) Store(_ context.Context, token string) error {
	c.stored = append(c.stored, token)
	return nil
}

// TestGlobalsStoreNoTokenByDefault pins the direction of the seam: a command
// tree built without an explicit custodian — which is every one a test builds,
// including tests not written yet — stores nothing, so running the CLI in
// process never writes into the login Keychain of whoever ran the tests.
func TestGlobalsStoreNoTokenByDefault(t *testing.T) {
	if g := newGlobals(); g.tokenCustody != nil {
		t.Errorf("tokenCustody = %#v, want nil", g.tokenCustody)
	}
}

func TestWithTokenCustodySuppliesTheCustodian(t *testing.T) {
	custody := tokencustody.NewNoop()

	if g := newGlobals(withTokenCustody(custody)); g.tokenCustody != custody {
		t.Errorf("tokenCustody = %#v, want the supplied custodian", g.tokenCustody)
	}
}

// A real `tumika token rotate` hands the token it prints to the custodian the
// command tree carries, which is how the platform secret store is reached at
// all — withDaemon is the only place the daemon's Options are built.
func TestTokenRotateHandsThePrintedTokenToTheCustodian(t *testing.T) {
	useTestKeyCustody(t)
	custody := &recordingCustodian{}

	cmd := newRootCmd(withTokenCustody(custody))
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"--home", t.TempDir(), "token", "rotate", "--quiet"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("token rotate: %v", err)
	}

	token := strings.TrimSpace(out.String())
	if !strings.HasPrefix(token, "tmk_") {
		t.Fatalf("no token was printed:\n%s", out.String())
	}
	if len(custody.stored) != 1 || custody.stored[0] != token {
		t.Errorf("custodian received %d token(s), want the printed one", len(custody.stored))
	}
}
