// Package anthropicapi drives the Anthropic HTTP API with an API key.
//
// It exists as much for the abstraction as for the capability. A provider seam
// with one implementation is a guess; this one implements neither Installer nor
// InteractiveAuthenticator, which is precisely the asymmetry the
// capability-by-interface design exists to handle. If the seam were quietly
// Claude-CLI-shaped, this package would not compile against it.
package anthropicapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/tumika/tumika/source/internal/domain"
	"github.com/tumika/tumika/source/internal/platform/logging"
)

// ID is the provider's stable identifier.
const ID = "anthropic-api"

// DefaultBaseURL is Anthropic's API.
const DefaultBaseURL = "https://api.anthropic.com"

// apiVersion is the value of the anthropic-version header. The API requires it
// on every request and refuses the request without it.
const apiVersion = "2023-06-01"

// keyPrefix is what an Anthropic API key starts with. Used for shape validation
// only — a well-formed key that has been revoked still fails at Verify, which is
// the only source of truth.
const keyPrefix = "sk-ant-"

// minKeyLength is a floor, not a specification: enough to reject an obvious
// paste error without asserting a format Anthropic has not promised.
const minKeyLength = 24

// verifyTimeout bounds the verification request. Long enough for a slow network,
// short enough that a hung endpoint does not hold up a login.
const verifyTimeout = 15 * time.Second

// Driver implements Provider, HealthChecker and StaticAuthenticator — and
// deliberately nothing else.
type Driver struct {
	baseURL string
	client  *http.Client
}

// Option configures the driver.
type Option func(*Driver)

// WithBaseURL points the driver at a different endpoint. For tests, and for a
// proxy or gateway deployment.
func WithBaseURL(url string) Option {
	return func(d *Driver) { d.baseURL = strings.TrimSuffix(url, "/") }
}

// WithHTTPClient replaces the HTTP client.
func WithHTTPClient(c *http.Client) Option {
	return func(d *Driver) { d.client = c }
}

// New builds the driver.
func New(opts ...Option) *Driver {
	d := &Driver{
		baseURL: DefaultBaseURL,
		client:  &http.Client{Timeout: verifyTimeout},
	}
	for _, opt := range opts {
		opt(d)
	}
	return d
}

func (d *Driver) Descriptor() domain.Descriptor {
	return domain.Descriptor{
		ID:          ID,
		DisplayName: "Anthropic API",
		Kind:        domain.ProviderKindHTTP,
		AuthMethods: []domain.AuthMethod{domain.AuthAPIKey},
		// Nothing is installed: this provider is an HTTP endpoint, which is why
		// it implements no Installer at all rather than one that refuses.
		Managed: false,
	}
}

// Preflight is trivially ready. There is no binary to find and no version to
// check — the whole provider is a URL, and whether that URL answers is what
// Verify is for.
func (d *Driver) Preflight(context.Context) (domain.Preflight, error) {
	return domain.Preflight{
		Ready:   true,
		Details: map[string]string{"base_url": d.baseURL},
	}, nil
}

func (d *Driver) AcceptedMethods() []domain.AuthMethod {
	return []domain.AuthMethod{domain.AuthAPIKey}
}

// ValidateSecret checks shape and touches no network, so an obviously wrong
// paste is rejected immediately rather than after a round trip that fails for a
// reason the operator has to decode.
func (d *Driver) ValidateSecret(method domain.AuthMethod, secret string) error {
	if method != domain.AuthAPIKey {
		return fmt.Errorf("%w: %s accepts %q, not %q",
			domain.ErrCredentialInvalid, ID, domain.AuthAPIKey, method)
	}

	switch {
	case strings.TrimSpace(secret) != secret:
		return fmt.Errorf("%w: the key has leading or trailing whitespace", domain.ErrCredentialInvalid)
	case secret == "":
		return fmt.Errorf("%w: no key was supplied", domain.ErrCredentialInvalid)
	case !strings.HasPrefix(secret, keyPrefix):
		return fmt.Errorf("%w: an Anthropic API key starts with %q", domain.ErrCredentialInvalid, keyPrefix)
	case len(secret) < minKeyLength:
		return fmt.Errorf("%w: the key is too short to be complete", domain.ErrCredentialInvalid)
	}
	return nil
}

func (d *Driver) Materialize(method domain.AuthMethod, secret string) (domain.Credential, error) {
	if err := d.ValidateSecret(method, secret); err != nil {
		return domain.Credential{}, err
	}
	return domain.Credential{
		ProviderID: ID,
		Kind:       domain.CredentialAPIKey,
		Secret:     secret,
		Meta: domain.CredentialMeta{
			Hint:   hint(secret),
			Status: string(domain.CredentialUnverified),
		},
	}, nil
}

// Verify calls the cheapest authenticated endpoint there is.
//
// GET /v1/models costs nothing, needs no model choice and no token budget, and
// answers the only question being asked: does this key work. A completion
// request would also work and would bill for it.
func (d *Driver) Verify(ctx context.Context, c domain.Credential) (domain.CredentialMeta, error) {
	meta := domain.CredentialMeta{Hint: hint(c.Secret), Status: string(domain.CredentialUnverified)}

	if c.Secret == "" {
		meta.Status = string(domain.CredentialInvalid)
		meta.LastVerifyError = "no key stored"
		return meta, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.baseURL+"/v1/models", nil)
	if err != nil {
		return meta, fmt.Errorf("build verification request: %w", err)
	}
	req.Header.Set("x-api-key", c.Secret)
	req.Header.Set("anthropic-version", apiVersion)

	resp, err := d.client.Do(req)
	if err != nil {
		// The check could not be carried out. That is an ERROR, not a verdict:
		// marking a credential invalid because the network was down would
		// revoke a working key over a transient failure.
		return meta, fmt.Errorf("reach %s: %w", d.baseURL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	now := time.Now().UTC()
	meta.LastVerifiedAt = &now

	switch {
	case resp.StatusCode == http.StatusOK:
		meta.Status = string(domain.CredentialActive)
		return meta, nil

	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		// A verdict, not a failure: the provider has told us the key is no good.
		meta.Status = string(domain.CredentialInvalid)
		meta.LastVerifyError = describe(resp)
		return meta, nil

	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		// Says nothing about the key. Leave the status alone and report an
		// error so the caller retries rather than revoking.
		return meta, fmt.Errorf("%s returned %s: %s", d.baseURL, resp.Status, describe(resp))

	default:
		return meta, fmt.Errorf("%s returned an unexpected %s: %s", d.baseURL, resp.Status, describe(resp))
	}
}

// describe extracts the provider's own error message, bounded and redacted.
//
// The message is stored in provider_credentials.last_verify_error and shown to
// the operator, so it leaves the process — which makes it credential-carrying
// surface. A provider that echoes the rejected key back in its error body (which
// is exactly the kind of thing an authentication endpoint does) would otherwise
// put a live key into the database in the clear. Redacting here rather than
// trusting the remote end is the only version of this that holds.
func describe(resp *http.Response) string {
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if err != nil || len(body) == 0 {
		return resp.Status
	}

	var payload struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err == nil && payload.Error.Message != "" {
		return logging.Redact(payload.Error.Message)
	}
	return logging.Redact(strings.TrimSpace(string(body)))
}

// hint is the only part of a key that may be shown or stored in the clear.
//
// The last four characters, which is enough for an operator to tell two keys
// apart and useless to anyone else. See
// .agents/rules/never-log-or-return-a-credential-secret.md.
func hint(secret string) string {
	const shown = 4
	if len(secret) <= shown {
		return ""
	}
	return "…" + secret[len(secret)-shown:]
}

// Compile-time proof of exactly which capabilities this driver claims. If one is
// ever dropped, this fails here rather than at the registry's runtime check.
var (
	_ interface {
		Descriptor() domain.Descriptor
		Preflight(context.Context) (domain.Preflight, error)
	} = (*Driver)(nil)
	_ interface {
		Verify(context.Context, domain.Credential) (domain.CredentialMeta, error)
	} = (*Driver)(nil)
)
