package domain

import "time"

// Credential kinds. These are the values stored in provider_credentials.kind
// and are part of the sealing AAD, so changing one is a migration.
const (
	// CredentialOAuthToken is the ~1-year subscription token minted by
	// `claude setup-token`.
	//
	// gosec's G101 flags this because the identifier contains "credential" and
	// "token" and the value is a string literal. It is a discriminator — the
	// value written to provider_credentials.kind and bound into the sealing AAD
	// — not a secret. Suppressed narrowly rather than by disabling the rule,
	// which is worth keeping for the case it is actually looking for.
	CredentialOAuthToken = "oauth_token" // #nosec G101 -- a credential kind label, not a credential
	// CredentialAPIKey is a provider API key.
	CredentialAPIKey = "api_key"
)

// CredentialStatus is the lifecycle of a stored credential.
type CredentialStatus string

const (
	// CredentialUnverified is sealed and stored but not yet proven to work.
	CredentialUnverified CredentialStatus = "unverified"
	// CredentialActive verified successfully against its provider.
	CredentialActive CredentialStatus = "active"
	// CredentialInvalid was rejected by its provider.
	CredentialInvalid CredentialStatus = "invalid"
	// CredentialExpired passed its expiry, or was rejected in a way that says so.
	CredentialExpired CredentialStatus = "expired"
	// CredentialRevoked was deleted by the operator.
	CredentialRevoked CredentialStatus = "revoked"
)

// Live reports whether a credential occupies the one live slot a provider has
// for a given kind. It is the Go statement of the partial unique index in
// 0001_init.sql, and the two must agree — a provider may accumulate any number
// of invalid, expired or revoked credentials, but only one that is in use or
// about to be.
func (s CredentialStatus) Live() bool {
	return s == CredentialActive || s == CredentialUnverified
}

// Credential carries the plaintext secret. It never crosses the API boundary,
// is never logged, and is deliberately not JSON-tagged — see
// .agents/rules/never-log-or-return-a-credential-secret.md.
type Credential struct {
	ID         int64
	ProviderID string
	Kind       string
	Secret     string
	Meta       CredentialMeta
}

// CredentialMeta is the non-secret half, and the only half that is serialised.
type CredentialMeta struct {
	Hint             string     `json:"hint,omitempty"`
	AccountEmail     string     `json:"account_email,omitempty"`
	Status           string     `json:"status"`
	IssuedAt         *time.Time `json:"issued_at,omitempty"`
	ExpiresAt        *time.Time `json:"expires_at,omitempty"`
	ExpiryIsEstimate bool       `json:"expiry_is_estimate,omitempty"`
	LastVerifiedAt   *time.Time `json:"last_verified_at,omitempty"`
	LastVerifyError  string     `json:"last_verify_error,omitempty"`
}

// SealedCredential is the stored form: ciphertext plus the metadata needed to
// open it again. This is what the repository reads and writes; the plaintext
// exists only above it, in the service layer.
type SealedCredential struct {
	ID         int64
	ProviderID string
	Kind       string

	Ciphertext []byte
	Nonce      []byte
	// KeyRef records which custody backend sealed this row
	// ("keychain:…", "systemd-creds:…", "file:…"), so re-keying after a
	// platform change is a query rather than an excavation (ADR-0002).
	KeyRef string
	Cipher string

	Meta      CredentialMeta
	CreatedAt time.Time
	UpdatedAt time.Time
}

// AAD is the additional authenticated data bound to this credential's
// ciphertext. Binding it to the row means ciphertext cannot be transplanted
// between rows: opening it under a different identity fails authentication
// rather than yielding a working token for the wrong provider.
func (c SealedCredential) AAD() []byte {
	return []byte(c.ProviderID + "|" + c.Kind)
}
