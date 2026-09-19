//go:build darwin

package tokencustody

import (
	"context"
	"fmt"

	"github.com/zalando/go-keyring"
)

// Keychain identifiers for the API token. They are constants because an
// operator looks the entry up by name:
//
//	security find-generic-password -s tumika -a api-token -w
//
// The service matches the master key's entry (see platform/secrets); the
// account is what separates the two.
const (
	keychainService = "tumika"
	keychainAccount = "api-token"
)

// keychainCustodian writes the API token to the macOS Keychain.
//
// set is a field rather than a direct call to keyring.Set so tests can exercise
// this without an entry appearing in the Keychain of whoever ran them.
type keychainCustodian struct {
	set func(service, account, secret string) error
}

// New returns the Custodian for this platform: the macOS Keychain.
func New() Custodian {
	return &keychainCustodian{set: keyring.Set}
}

func (c *keychainCustodian) Store(ctx context.Context, token string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := c.set(keychainService, keychainAccount, token); err != nil {
		// The token itself never reaches the message — see
		// agentic/rules/never-log-or-return-a-credential-secret.md.
		return fmt.Errorf("store the api token in the macOS Keychain (%s/%s): %w",
			keychainService, keychainAccount, err)
	}
	return nil
}
