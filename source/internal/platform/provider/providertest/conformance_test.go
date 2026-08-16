package providertest_test

import (
	"context"
	"strings"
	"testing"

	"github.com/tumika/tumika/source/internal/domain"
	"github.com/tumika/tumika/source/internal/platform/provider/providertest"
)

// The suite is run against every driver, so a bug in it would weaken every one
// of those runs at once. These exercise it against drivers built to be correct
// and to be wrong.

type conformingDriver struct{}

func (conformingDriver) Descriptor() domain.Descriptor {
	return domain.Descriptor{
		ID:          "conforming",
		DisplayName: "Conforming",
		Kind:        domain.ProviderKindHTTP,
		AuthMethods: []domain.AuthMethod{domain.AuthAPIKey},
	}
}

func (conformingDriver) Preflight(context.Context) (domain.Preflight, error) {
	return domain.Preflight{Ready: true}, nil
}

func (conformingDriver) Verify(context.Context, domain.Credential) (domain.CredentialMeta, error) {
	return domain.CredentialMeta{}, nil
}

func (conformingDriver) AcceptedMethods() []domain.AuthMethod {
	return []domain.AuthMethod{domain.AuthAPIKey}
}

func (conformingDriver) ValidateSecret(method domain.AuthMethod, secret string) error {
	// A prefix check, not just a length one: the suite submits
	// "not-a-credential", which is long enough to pass a length test and is
	// obviously not a credential.
	if method != domain.AuthAPIKey || !strings.HasPrefix(secret, "key-") {
		return domain.ErrCredentialInvalid
	}
	return nil
}

func (conformingDriver) Materialize(_ domain.AuthMethod, secret string) (domain.Credential, error) {
	return domain.Credential{Secret: secret}, nil
}

func TestConformingDriverPasses(t *testing.T) {
	providertest.Conformance(t, conformingDriver{})
}

// An interactive driver exercises the other side of every branch: the suite must
// pass a driver whose capabilities are the mirror of the one above.
type interactiveDriver struct{ conformingDriver }

func (interactiveDriver) Descriptor() domain.Descriptor {
	return domain.Descriptor{
		ID:          "interactive",
		DisplayName: "Interactive",
		Kind:        domain.ProviderKindCLI,
		AuthMethods: []domain.AuthMethod{domain.AuthInteractiveCLI, domain.AuthManualToken},
		Managed:     true,
	}
}

func (interactiveDriver) AcceptedMethods() []domain.AuthMethod {
	return []domain.AuthMethod{domain.AuthManualToken}
}

func (interactiveDriver) ValidateSecret(method domain.AuthMethod, secret string) error {
	if method != domain.AuthManualToken || !strings.HasPrefix(secret, "token-") {
		return domain.ErrCredentialInvalid
	}
	return nil
}

func (interactiveDriver) Begin(context.Context, string) (<-chan domain.LoginEvent, error) {
	return nil, nil
}
func (interactiveDriver) Submit(context.Context, string, string) error { return nil }
func (interactiveDriver) Cancel(context.Context, string) error         { return nil }

func (interactiveDriver) Install(context.Context, string) (domain.InstallResult, error) {
	return domain.InstallResult{}, nil
}
func (interactiveDriver) Installed(context.Context) ([]string, error) { return nil, nil }
func (interactiveDriver) Prune(context.Context, int) error            { return nil }

func TestInteractiveDriverPasses(t *testing.T) {
	providertest.Conformance(t, interactiveDriver{})
}

// The registry's validation is what the suite leans on for capability
// correctness, and registry_test.go exercises every way a descriptor can
// disagree with its interfaces. What is checked here is that the suite passes
// two drivers with opposite capability sets — static-only and
// interactive-plus-installer — so neither branch of its checks is dead.
