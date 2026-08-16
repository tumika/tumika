// Package provider is the seam between tumika and the LLM backends it drives.
//
// A driver declares what it can do by WHICH INTERFACES IT IMPLEMENTS, and the
// registry discovers that by type assertion — never by a boolean field, never by
// a switch on the provider ID, and never by a method that exists only to return
// "not supported". See
// .agents/rules/provider-drivers-declare-capabilities-by-interface.md.
package provider

import (
	"context"
	"fmt"
	"slices"

	"github.com/tumika/tumika/source/internal/domain"
)

// Provider is the mandatory interface. Every driver implements it.
type Provider interface {
	Descriptor() domain.Descriptor
	Preflight(ctx context.Context) (domain.Preflight, error)
}

// HealthChecker is mandatory too: a credential tumika cannot verify is a
// credential tumika cannot honestly report on.
type HealthChecker interface {
	// Verify uses the credential against the provider and reports what it
	// learned. A rejected credential is a RESULT, not an error — it returns
	// metadata with a failed status. An error means the check itself could not
	// be carried out.
	Verify(ctx context.Context, c domain.Credential) (domain.CredentialMeta, error)
}

// StaticAuthenticator is optional: the caller already holds the secret, so there
// is no session, only a submission.
type StaticAuthenticator interface {
	// AcceptedMethods is the static subset of the descriptor's AuthMethods.
	AcceptedMethods() []domain.AuthMethod
	// ValidateSecret checks shape only, and touches no network. It exists so an
	// obviously wrong paste is rejected immediately rather than after a round
	// trip that will fail confusingly.
	ValidateSecret(method domain.AuthMethod, secret string) error
	// Materialize turns a validated secret into a credential.
	Materialize(method domain.AuthMethod, secret string) (domain.Credential, error)
}

// InteractiveAuthenticator is optional: obtaining the credential needs a
// multi-step session, because the provider will only hand one over to a human at
// a browser.
//
// Declared now and implemented at the PTY step. It is here because the registry
// must be able to discover its absence today — that is what makes
// `POST …/login` return a documented 400 on a provider that has no such flow.
type InteractiveAuthenticator interface {
	Begin(ctx context.Context, sessionID string) (<-chan domain.LoginEvent, error)
	Submit(ctx context.Context, sessionID string, input string) error
	Cancel(ctx context.Context, sessionID string) error
}

// Installer is optional: only for providers tumika vendors a binary for.
type Installer interface {
	Install(ctx context.Context, version string) (domain.InstallResult, error)
	Installed(ctx context.Context) ([]string, error)
	Prune(ctx context.Context, keep int) error
}

// Registry holds the drivers and answers capability questions about them.
type Registry struct {
	byID  map[string]Provider
	order []string
}

// NewRegistry validates every driver and returns the registry.
//
// Validation happens at construction, so a driver whose descriptor disagrees
// with its interfaces stops the daemon at startup rather than producing a client
// that offers a flow the daemon will reject. That correspondence cannot be
// checked by the compiler, which is exactly why it is checked here.
func NewRegistry(providers ...Provider) (*Registry, error) {
	r := &Registry{byID: make(map[string]Provider, len(providers))}

	for _, p := range providers {
		d := p.Descriptor()
		if err := validate(p, d); err != nil {
			return nil, err
		}
		if _, dup := r.byID[d.ID]; dup {
			return nil, fmt.Errorf("provider %q is registered twice", d.ID)
		}
		r.byID[d.ID] = p
		r.order = append(r.order, d.ID)
	}

	slices.Sort(r.order)
	return r, nil
}

func validate(p Provider, d domain.Descriptor) error {
	switch {
	case d.ID == "":
		return fmt.Errorf("a provider has no ID")
	case d.DisplayName == "":
		return fmt.Errorf("provider %q has no display name", d.ID)
	case d.Kind != domain.ProviderKindCLI && d.Kind != domain.ProviderKindHTTP:
		return fmt.Errorf("provider %q has kind %q, want %q or %q",
			d.ID, d.Kind, domain.ProviderKindCLI, domain.ProviderKindHTTP)
	case len(d.AuthMethods) == 0:
		return fmt.Errorf("provider %q declares no auth methods, so no credential could ever be supplied", d.ID)
	}

	// Verify is how a stored credential is ever confirmed to work; a driver
	// without it could only ever report "unverified".
	if _, ok := p.(HealthChecker); !ok {
		return fmt.Errorf("provider %q does not implement HealthChecker", d.ID)
	}

	static, hasStatic := p.(StaticAuthenticator)
	_, hasInteractive := p.(InteractiveAuthenticator)

	var declaredStatic []domain.AuthMethod
	for _, m := range d.AuthMethods {
		if !m.Valid() {
			return fmt.Errorf("provider %q declares unknown auth method %q", d.ID, m)
		}
		if !m.Interactive() {
			declaredStatic = append(declaredStatic, m)
		}
	}

	// Each direction of the correspondence fails differently and both are
	// silent: a descriptor that overstates its methods produces a UI offering a
	// flow the daemon rejects, and one that understates them hides a working
	// path.
	if len(declaredStatic) > 0 && !hasStatic {
		return fmt.Errorf("provider %q declares static auth methods %v but does not implement StaticAuthenticator",
			d.ID, declaredStatic)
	}
	if hasStatic && len(declaredStatic) == 0 {
		return fmt.Errorf("provider %q implements StaticAuthenticator but declares no static auth method", d.ID)
	}
	if d.RequiresInteractiveAuth() && !hasInteractive {
		return fmt.Errorf("provider %q declares %q but does not implement InteractiveAuthenticator",
			d.ID, domain.AuthInteractiveCLI)
	}
	if hasInteractive && !d.RequiresInteractiveAuth() {
		return fmt.Errorf("provider %q implements InteractiveAuthenticator but does not declare %q",
			d.ID, domain.AuthInteractiveCLI)
	}

	// Managed says tumika installs a binary for this provider, and clients render
	// an install affordance from it. The conformance suite already required the
	// pair to agree; checking it here makes a mismatch a startup failure rather
	// than something only a test would catch.
	if _, isInstaller := p.(Installer); isInstaller != d.Managed {
		return fmt.Errorf("provider %q has Managed=%v but Installer implemented=%v; they must agree",
			d.ID, d.Managed, isInstaller)
	}

	if hasStatic {
		accepted := static.AcceptedMethods()
		for _, m := range declaredStatic {
			if !slices.Contains(accepted, m) {
				return fmt.Errorf("provider %q declares %q but AcceptedMethods does not include it", d.ID, m)
			}
		}
		for _, m := range accepted {
			if !slices.Contains(declaredStatic, m) {
				return fmt.Errorf("provider %q accepts %q but does not declare it", d.ID, m)
			}
		}
	}

	return nil
}

// Get returns a provider, or domain.ErrNotFound.
func (r *Registry) Get(id string) (Provider, error) {
	p, ok := r.byID[id]
	if !ok {
		return nil, fmt.Errorf("%w: provider %q", domain.ErrNotFound, id)
	}
	return p, nil
}

// List returns every provider, ordered by ID so the API is stable.
func (r *Registry) List() []Provider {
	out := make([]Provider, 0, len(r.order))
	for _, id := range r.order {
		out = append(out, r.byID[id])
	}
	return out
}

// IDs returns every registered provider ID, ordered.
func (r *Registry) IDs() []string { return slices.Clone(r.order) }

// HealthChecker returns the driver's verifier. Mandatory, so this only fails for
// an unknown provider.
func (r *Registry) HealthChecker(id string) (HealthChecker, error) {
	p, err := r.Get(id)
	if err != nil {
		return nil, err
	}
	return p.(HealthChecker), nil // guaranteed by validate
}

// StaticAuthenticator returns the driver's static auth, or
// domain.ErrInteractiveAuthRequired if it only supports a session.
func (r *Registry) StaticAuthenticator(id string) (StaticAuthenticator, error) {
	p, err := r.Get(id)
	if err != nil {
		return nil, err
	}
	sa, ok := p.(StaticAuthenticator)
	if !ok {
		return nil, fmt.Errorf("%w: %q accepts a credential only through a login session",
			domain.ErrInteractiveAuthRequired, id)
	}
	return sa, nil
}

// InteractiveAuthenticator returns the driver's login flow, or
// domain.ErrInteractiveAuthUnsupported.
func (r *Registry) InteractiveAuthenticator(id string) (InteractiveAuthenticator, error) {
	p, err := r.Get(id)
	if err != nil {
		return nil, err
	}
	ia, ok := p.(InteractiveAuthenticator)
	if !ok {
		return nil, fmt.Errorf("%w: %q", domain.ErrInteractiveAuthUnsupported, id)
	}
	return ia, nil
}

// Installer returns the driver's installer, or domain.ErrInstallUnsupported.
func (r *Registry) Installer(id string) (Installer, error) {
	p, err := r.Get(id)
	if err != nil {
		return nil, err
	}
	inst, ok := p.(Installer)
	if !ok {
		return nil, fmt.Errorf("%w: %q installs nothing", domain.ErrInstallUnsupported, id)
	}
	return inst, nil
}
