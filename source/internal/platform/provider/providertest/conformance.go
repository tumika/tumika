// Package providertest holds the conformance suite every driver must pass.
//
// It lives in a normal package rather than a _test file so each driver's own
// tests can call it — the point is that the suite is written ONCE and run
// against every implementation, so a second driver cannot quietly diverge from
// the first. Modelled on net/http/httptest: importing "testing" here is
// deliberate.
package providertest

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/tumika/tumika/source/internal/domain"
	"github.com/tumika/tumika/source/internal/platform/provider"
)

// Conformance runs every check that must hold for any driver.
func Conformance(t *testing.T, p provider.Provider) {
	t.Helper()

	t.Run("descriptor is well formed", func(t *testing.T) { descriptorIsWellFormed(t, p) })
	t.Run("registry accepts it", func(t *testing.T) { registryAccepts(t, p) })
	t.Run("capabilities match the descriptor", func(t *testing.T) { capabilitiesMatch(t, p) })
	t.Run("unsupported capabilities are refused by sentinel", func(t *testing.T) { unsupportedRefused(t, p) })
	t.Run("preflight answers", func(t *testing.T) { preflightAnswers(t, p) })
	t.Run("static auth rejects rubbish without a network call", func(t *testing.T) { staticRejectsRubbish(t, p) })
}

func descriptorIsWellFormed(t *testing.T, p provider.Provider) {
	d := p.Descriptor()

	if d.ID == "" {
		t.Error("ID is empty")
	}
	if d.DisplayName == "" {
		t.Error("DisplayName is empty")
	}
	if d.Kind != domain.ProviderKindCLI && d.Kind != domain.ProviderKindHTTP {
		t.Errorf("Kind = %q, want cli or http", d.Kind)
	}
	if len(d.AuthMethods) == 0 {
		t.Error("no auth methods declared, so no credential could ever be supplied")
	}
	for _, m := range d.AuthMethods {
		if !m.Valid() {
			t.Errorf("unknown auth method %q", m)
		}
	}
	// The descriptor is what a client branches on before rendering anything.
	if slices.Contains(d.AuthMethods, domain.AuthInteractiveCLI) != d.RequiresInteractiveAuth() {
		t.Error("RequiresInteractiveAuth disagrees with AuthMethods")
	}
}

// The registry's validation is the real gate; a driver that cannot be registered
// cannot be used, so every driver must pass it.
func registryAccepts(t *testing.T, p provider.Provider) {
	if _, err := provider.NewRegistry(p); err != nil {
		t.Errorf("the registry refused this driver: %v", err)
	}
}

func capabilitiesMatch(t *testing.T, p provider.Provider) {
	d := p.Descriptor()

	if _, ok := p.(provider.HealthChecker); !ok {
		t.Error("HealthChecker is mandatory: without it a credential could only ever be unverified")
	}

	static, hasStatic := p.(provider.StaticAuthenticator)
	var declaredStatic []domain.AuthMethod
	for _, m := range d.AuthMethods {
		if !m.Interactive() {
			declaredStatic = append(declaredStatic, m)
		}
	}

	if len(declaredStatic) > 0 && !hasStatic {
		t.Errorf("declares static methods %v but does not implement StaticAuthenticator", declaredStatic)
	}
	if hasStatic {
		accepted := static.AcceptedMethods()
		if len(accepted) != len(declaredStatic) {
			t.Errorf("AcceptedMethods %v does not match the declared static methods %v", accepted, declaredStatic)
		}
		for _, m := range declaredStatic {
			if !slices.Contains(accepted, m) {
				t.Errorf("declares %q but AcceptedMethods omits it", m)
			}
		}
	}

	if _, hasInteractive := p.(provider.InteractiveAuthenticator); hasInteractive != d.RequiresInteractiveAuth() {
		t.Errorf("InteractiveAuthenticator implemented = %v but RequiresInteractiveAuth = %v",
			hasInteractive, d.RequiresInteractiveAuth())
	}

	// Managed means tumika installs a binary. Anything else claiming it would
	// leave a client offering an install button that cannot work.
	if _, isInstaller := p.(provider.Installer); isInstaller != d.Managed {
		t.Errorf("Installer implemented = %v but Managed = %v", isInstaller, d.Managed)
	}
}

// A capability a driver lacks must be refused with the documented sentinel, so
// the API can answer with a stable code instead of a nil-pointer panic three
// layers down.
func unsupportedRefused(t *testing.T, p provider.Provider) {
	registry, err := provider.NewRegistry(p)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	id := p.Descriptor().ID

	if _, isInstaller := p.(provider.Installer); !isInstaller {
		if _, err := registry.Installer(id); !errors.Is(err, domain.ErrInstallUnsupported) {
			t.Errorf("Installer(%q) = %v, want domain.ErrInstallUnsupported", id, err)
		}
	}
	if !p.Descriptor().RequiresInteractiveAuth() {
		if _, err := registry.InteractiveAuthenticator(id); !errors.Is(err, domain.ErrInteractiveAuthUnsupported) {
			t.Errorf("InteractiveAuthenticator(%q) = %v, want domain.ErrInteractiveAuthUnsupported", id, err)
		}
	}
	if _, hasStatic := p.(provider.StaticAuthenticator); !hasStatic {
		if _, err := registry.StaticAuthenticator(id); !errors.Is(err, domain.ErrInteractiveAuthRequired) {
			t.Errorf("StaticAuthenticator(%q) = %v, want domain.ErrInteractiveAuthRequired", id, err)
		}
	}
}

func preflightAnswers(t *testing.T, p provider.Provider) {
	pf, err := p.Preflight(context.Background())
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	// Not-ready without a reason leaves an operator with nothing to act on.
	if !pf.Ready && len(pf.Blockers) == 0 {
		t.Error("Preflight reports not ready but names no blocker")
	}
}

func staticRejectsRubbish(t *testing.T, p provider.Provider) {
	static, ok := p.(provider.StaticAuthenticator)
	if !ok {
		t.Skip("this driver has no static auth")
	}

	for _, method := range static.AcceptedMethods() {
		for _, rubbish := range []string{"", " ", "not-a-credential"} {
			if err := static.ValidateSecret(method, rubbish); err == nil {
				t.Errorf("ValidateSecret(%q, %q) accepted it", method, rubbish)
			}
		}
		// An unknown method must be refused too, or a client could smuggle a
		// secret in under a method the driver never agreed to.
		if err := static.ValidateSecret(domain.AuthMethod("made-up"), "whatever"); err == nil {
			t.Error("ValidateSecret accepted an unknown auth method")
		}
	}
}
