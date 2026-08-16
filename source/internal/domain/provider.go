package domain

import "time"

// AuthMethod is how a credential for a provider is obtained.
type AuthMethod string

const (
	// AuthAPIKey is static: the caller already holds the key.
	AuthAPIKey AuthMethod = "api_key"
	// AuthManualToken is static: the user obtains the token out of band — by
	// running `claude setup-token` themselves — and submits it.
	AuthManualToken AuthMethod = "manual_token"
	// AuthInteractiveCLI needs a login session: a browser approval and a code
	// exchange driven through a PTY.
	AuthInteractiveCLI AuthMethod = "interactive_cli"
)

// Interactive reports whether obtaining this credential requires a login
// session rather than a single submission.
func (m AuthMethod) Interactive() bool { return m == AuthInteractiveCLI }

// Valid reports whether m is a known method.
func (m AuthMethod) Valid() bool {
	switch m {
	case AuthAPIKey, AuthManualToken, AuthInteractiveCLI:
		return true
	default:
		return false
	}
}

// Provider kinds.
const (
	// ProviderKindCLI is a provider driven as a subprocess.
	ProviderKindCLI = "cli"
	// ProviderKindHTTP is a provider called over HTTP.
	ProviderKindHTTP = "http"
)

// Descriptor is the static, public description of a provider — what a client
// reads to decide how to render it, before any credential exists.
type Descriptor struct {
	ID          string       `json:"id"`
	DisplayName string       `json:"display_name"`
	Kind        string       `json:"kind"`         // "cli" | "http"
	AuthMethods []AuthMethod `json:"auth_methods"` // ordered, preferred first
	Managed     bool         `json:"managed"`      // tumika installs a binary for it
}

// RequiresInteractiveAuth tells a client whether to drive the login-session
// endpoints or simply submit a secret.
//
// It is derived from AuthMethods rather than stored, so it cannot disagree with
// them. The driver's AuthMethods must in turn match the interfaces it actually
// implements — see
// .agents/rules/provider-drivers-declare-capabilities-by-interface.md.
func (d Descriptor) RequiresInteractiveAuth() bool {
	for _, m := range d.AuthMethods {
		if m.Interactive() {
			return true
		}
	}
	return false
}

// Provider is the persisted, mutable half of a provider: what the registry
// seeded and what the operator has since changed. The immutable half is the
// Descriptor, which comes from the driver rather than the database.
type Provider struct {
	ID          string
	DisplayName string
	Kind        string
	Enabled     bool
	// Config is provider-specific settings as JSON. It is opaque to the
	// repository; only the driver interprets it.
	Config    []byte
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Preflight is a driver's report on whether it can be used right now, and if
// not, what an operator would have to do about it.
//
// Separate from health: health describes the daemon, preflight describes one
// provider's readiness, and the answer is usually actionable rather than
// alarming ("Claude Code is not installed" is a next step, not an incident).
type Preflight struct {
	Ready bool `json:"ready"`
	// Blockers are stated in the operator's terms, not the driver's.
	Blockers []string `json:"blockers,omitempty"`
	// Details is driver-specific and purely informational — an installed
	// version, a resolved binary path. Never credential material.
	Details map[string]string `json:"details,omitempty"`
}

// InstallResult describes what an Installer did.
type InstallResult struct {
	Version string `json:"version"`
	Path    string `json:"path"`
	// AlreadyPresent distinguishes "installed it" from "it was already there",
	// which matters because installing is expensive and pinned.
	AlreadyPresent bool `json:"already_present"`
}

// ProviderView is a provider as a client sees it: what it is, what it can do,
// and what tumika currently holds for it.
//
// The descriptor is embedded, so a client reads auth_methods and
// requires_interactive_auth from the same object it reads the credential state
// from — which is what lets it decide, in one look, whether to render a secret
// field or drive the login endpoints.
type ProviderView struct {
	Descriptor
	// RequiresInteractiveAuth is derived from AuthMethods and serialised
	// explicitly, so a client does not have to re-derive it.
	RequiresInteractiveAuth bool `json:"requires_interactive_auth"`
	Enabled                 bool `json:"enabled"`
	Selected                bool `json:"selected"`
	// Credential is the non-secret half of the live credential, or nil when
	// none is stored. The secret never appears here — see
	// .agents/rules/never-log-or-return-a-credential-secret.md.
	Credential *CredentialMeta `json:"credential,omitempty"`
}
