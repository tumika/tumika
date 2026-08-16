package provider_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tumika/tumika/source/internal/domain"
	"github.com/tumika/tumika/source/internal/platform/provider"
)

// fake is a driver whose capabilities are assembled per test, so the registry's
// validation can be exercised against every way a descriptor can disagree with
// its interfaces.
type fake struct{ d domain.Descriptor }

func (f fake) Descriptor() domain.Descriptor { return f.d }
func (f fake) Preflight(context.Context) (domain.Preflight, error) {
	return domain.Preflight{Ready: true}, nil
}

func (f fake) Verify(context.Context, domain.Credential) (domain.CredentialMeta, error) {
	return domain.CredentialMeta{}, nil
}

// noVerify lacks HealthChecker.
type noVerify struct{ d domain.Descriptor }

func (n noVerify) Descriptor() domain.Descriptor { return n.d }
func (n noVerify) Preflight(context.Context) (domain.Preflight, error) {
	return domain.Preflight{Ready: true}, nil
}

// withStatic adds StaticAuthenticator, accepting the methods given.
type withStatic struct {
	fake
	accepted []domain.AuthMethod
}

func (w withStatic) AcceptedMethods() []domain.AuthMethod { return w.accepted }
func (w withStatic) ValidateSecret(domain.AuthMethod, string) error {
	return nil
}

func (w withStatic) Materialize(domain.AuthMethod, string) (domain.Credential, error) {
	return domain.Credential{}, nil
}

// withInteractive adds InteractiveAuthenticator.
type withInteractive struct{ fake }

func (w withInteractive) Begin(context.Context, string) (<-chan domain.LoginEvent, error) {
	return nil, nil
}
func (w withInteractive) Submit(context.Context, string, string) error { return nil }
func (w withInteractive) Cancel(context.Context, string) error         { return nil }

// withInstaller adds Installer.
type withInstaller struct{ fake }

func (w withInstaller) Install(context.Context, string) (domain.InstallResult, error) {
	return domain.InstallResult{}, nil
}
func (w withInstaller) Installed(context.Context) ([]string, error) { return nil, nil }
func (w withInstaller) Prune(context.Context, int) error            { return nil }

func descriptor(methods ...domain.AuthMethod) domain.Descriptor {
	return domain.Descriptor{
		ID:          "fake",
		DisplayName: "Fake",
		Kind:        domain.ProviderKindHTTP,
		AuthMethods: methods,
	}
}

// The correspondence between a descriptor and the interfaces a driver actually
// implements cannot be checked by the compiler, and both directions fail
// silently: overstating produces a UI offering a flow the daemon rejects,
// understating hides a working path. So the registry checks it at construction,
// which turns it into a startup failure rather than a runtime surprise.
func TestRegistryRejectsADescriptorThatDisagreesWithItsInterfaces(t *testing.T) {
	tests := map[string]struct {
		p    provider.Provider
		want string
	}{
		"no HealthChecker": {
			p:    noVerify{d: descriptor(domain.AuthAPIKey)},
			want: "HealthChecker",
		},
		"declares a static method but has no StaticAuthenticator": {
			p:    fake{d: descriptor(domain.AuthAPIKey)},
			want: "StaticAuthenticator",
		},
		// Two rules are broken here at once — static auth implemented but not
		// declared, and interactive declared but not implemented. Either
		// message is correct; the check reports the static one first.
		"implements StaticAuthenticator but declares only interactive": {
			p: withStatic{
				fake:     fake{d: descriptor(domain.AuthInteractiveCLI)},
				accepted: []domain.AuthMethod{domain.AuthAPIKey},
			},
			want: "declares no static auth method",
		},
		"declares interactive but has no InteractiveAuthenticator": {
			p:    fake{d: descriptor(domain.AuthInteractiveCLI)},
			want: "InteractiveAuthenticator",
		},
		"implements InteractiveAuthenticator but does not declare it": {
			p:    withInteractive{fake: fake{d: descriptor(domain.AuthAPIKey)}},
			want: "StaticAuthenticator",
		},
		"AcceptedMethods omits a declared method": {
			p: withStatic{
				fake:     fake{d: descriptor(domain.AuthAPIKey, domain.AuthManualToken)},
				accepted: []domain.AuthMethod{domain.AuthAPIKey},
			},
			want: "AcceptedMethods",
		},
		"accepts a method it does not declare": {
			p: withStatic{
				fake:     fake{d: descriptor(domain.AuthAPIKey)},
				accepted: []domain.AuthMethod{domain.AuthAPIKey, domain.AuthManualToken},
			},
			want: "does not declare",
		},
		"no auth methods at all": {
			p:    fake{d: descriptor()},
			want: "no auth methods",
		},
		"unknown auth method": {
			p: withStatic{
				fake:     fake{d: descriptor(domain.AuthMethod("telepathy"))},
				accepted: []domain.AuthMethod{domain.AuthMethod("telepathy")},
			},
			want: "unknown auth method",
		},
		"no ID": {
			p:    fake{d: domain.Descriptor{DisplayName: "x", Kind: domain.ProviderKindHTTP, AuthMethods: []domain.AuthMethod{domain.AuthAPIKey}}},
			want: "no ID",
		},
		"bad kind": {
			p: withStatic{
				fake: fake{d: domain.Descriptor{
					ID: "fake", DisplayName: "Fake", Kind: "carrier-pigeon",
					AuthMethods: []domain.AuthMethod{domain.AuthAPIKey},
				}},
				accepted: []domain.AuthMethod{domain.AuthAPIKey},
			},
			want: "kind",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := provider.NewRegistry(tc.p)
			if err == nil {
				t.Fatal("the registry accepted a driver whose descriptor lies about its capabilities")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestRegistryAcceptsAConsistentDriver(t *testing.T) {
	p := withStatic{
		fake:     fake{d: descriptor(domain.AuthAPIKey)},
		accepted: []domain.AuthMethod{domain.AuthAPIKey},
	}

	r, err := provider.NewRegistry(p)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if got := r.IDs(); len(got) != 1 || got[0] != "fake" {
		t.Errorf("IDs = %v", got)
	}
	if _, err := r.HealthChecker("fake"); err != nil {
		t.Errorf("HealthChecker: %v", err)
	}
	if _, err := r.StaticAuthenticator("fake"); err != nil {
		t.Errorf("StaticAuthenticator: %v", err)
	}
}

func TestRegistryRejectsDuplicateIDs(t *testing.T) {
	p := withStatic{fake: fake{d: descriptor(domain.AuthAPIKey)}, accepted: []domain.AuthMethod{domain.AuthAPIKey}}

	if _, err := provider.NewRegistry(p, p); err == nil {
		t.Error("the registry accepted the same provider ID twice")
	}
}

// Capability questions about an absent capability answer with the documented
// sentinel, so the API returns a stable code rather than panicking three layers
// down.
func TestCapabilityLookupsUseSentinels(t *testing.T) {
	p := withStatic{fake: fake{d: descriptor(domain.AuthAPIKey)}, accepted: []domain.AuthMethod{domain.AuthAPIKey}}
	r, err := provider.NewRegistry(p)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	if _, err := r.Installer("fake"); !errors.Is(err, domain.ErrInstallUnsupported) {
		t.Errorf("Installer = %v, want ErrInstallUnsupported", err)
	}
	if _, err := r.InteractiveAuthenticator("fake"); !errors.Is(err, domain.ErrInteractiveAuthUnsupported) {
		t.Errorf("InteractiveAuthenticator = %v, want ErrInteractiveAuthUnsupported", err)
	}
	if _, err := r.Get("nope"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Get(unknown) = %v, want ErrNotFound", err)
	}
}

func TestManagedProvidersImplementInstaller(t *testing.T) {
	d := descriptor(domain.AuthAPIKey)
	d.Managed = true

	p := withInstaller{fake: fake{d: d}}
	// Managed with an Installer is fine; the conformance suite checks the pair
	// stays in step. Here we only need the registry not to reject it.
	if _, err := provider.NewRegistry(withStatic{fake: p.fake, accepted: []domain.AuthMethod{domain.AuthAPIKey}}); err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
}
