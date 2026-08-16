package claudecode

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/tumika/tumika/source/internal/domain"
	"github.com/tumika/tumika/source/internal/platform/logging"
)

// ID is the provider's stable identifier.
const ID = "claude-code"

// tokenPrefix is what `claude setup-token` mints.
//
// Shape validation only. A well-formed token that has been revoked still fails
// at Verify, which is the only source of truth — see
// .agents/rules/every-spawned-claude-process-is-credential-isolated.md.
const tokenPrefix = "sk-ant-oat01-" // #nosec G101 -- the public prefix of a token format, not a token

// minTokenLength is a floor, not a specification: enough to reject a truncated
// paste without asserting a length Anthropic has not promised.
const minTokenLength = 40

// oauthAuthMethod is the value `claude auth status --json` must report.
//
// Anything else means the CLI resolved a DIFFERENT credential than the one
// tumika supplied, and every other value on that chain bills at API rates.
const oauthAuthMethod = "oauth_token"

// assumedTokenLifetime is how long a subscription token is taken to last when it
// does not say.
//
// `claude setup-token` documents roughly a year and the token is opaque, so this
// is an estimate and is flagged as one. CredentialMonitorRunner re-verifying
// daily is the real detector; this only drives the warning that gives an
// operator notice before it stops working.
const assumedTokenLifetime = 365 * 24 * time.Hour

// verifyTimeout bounds each stage of verification. Generous, because stage two
// is a real (tiny) model call over the operator's connection.
const verifyTimeout = 60 * time.Second

// verifyPrompt is the cheapest thing that still proves the credential can reach
// a model: one turn, a handful of tokens, no tools.
const verifyPrompt = "Reply with the single word: ok"

// Driver drives the vendored Claude Code CLI with a subscription token.
//
// It implements Provider, HealthChecker, StaticAuthenticator and Installer — and
// not InteractiveAuthenticator, which arrives with the PTY login. That absence
// is discoverable: it is what makes `POST …/login` answer a documented 400
// today rather than hanging on a flow that does not exist.
type Driver struct {
	installer *Installer
	configDir string
	version   string
	timeout   time.Duration
	now       func() time.Time
}

// DriverOption configures the driver.
type DriverOption func(*Driver)

// WithPinnedVersion overrides the version the driver installs and executes.
// For tests; production passes buildinfo.PinnedClaudeCodeVersion.
func WithPinnedVersion(v string) DriverOption {
	return func(d *Driver) { d.version = v }
}

// WithVerifyTimeout bounds each verification stage.
func WithVerifyTimeout(t time.Duration) DriverOption {
	return func(d *Driver) { d.timeout = t }
}

// WithClock replaces the clock, so expiry estimates are assertable.
func WithClock(now func() time.Time) DriverOption {
	return func(d *Driver) { d.now = now }
}

// NewDriver builds the driver.
//
// providersRoot is where versions are installed; configDir is the isolated
// CLAUDE_CONFIG_DIR every spawned claude is given, so the daemon never reads or
// mutates the operator's own Claude Code configuration. version is the pin, and
// the caller passes it rather than the package naming it: the number lives in
// exactly one place, buildinfo.PinnedClaudeCodeVersion.
func NewDriver(providersRoot, configDir, version string, opts ...DriverOption) (*Driver, error) {
	inst, err := NewInstaller(providersRoot)
	if err != nil {
		return nil, err
	}
	return newDriver(inst, configDir, version, opts...)
}

func newDriver(inst *Installer, configDir, version string, opts ...DriverOption) (*Driver, error) {
	d := &Driver{
		installer: inst,
		configDir: configDir,
		version:   version,
		timeout:   verifyTimeout,
		now:       func() time.Time { return time.Now().UTC() },
	}
	for _, opt := range opts {
		opt(d)
	}

	// A driver with no config directory would fall back to the operator's own
	// ~/.claude, which is the isolation the rule exists to provide. Refused at
	// construction rather than discovered when a login rewrites their settings.
	if d.configDir == "" {
		return nil, errors.New("claudecode: no CLAUDE_CONFIG_DIR; a spawned claude would use the operator's own configuration")
	}
	if err := validateVersion(d.version); err != nil {
		return nil, fmt.Errorf("claudecode: pinned version: %w", err)
	}
	return d, nil
}

func (d *Driver) Descriptor() domain.Descriptor {
	return domain.Descriptor{
		ID:          ID,
		DisplayName: "Claude Code",
		Kind:        domain.ProviderKindCLI,
		// Only the static method today. interactive_cli is deliberately NOT
		// declared until the PTY flow exists: the registry validates the
		// descriptor against the implemented interfaces, so declaring it early
		// would fail at startup — which is the check working.
		AuthMethods: []domain.AuthMethod{domain.AuthManualToken},
		Managed:     true,
	}
}

// Preflight reports whether the pinned binary is actually there to run.
//
// A blocker here is a next step, not an incident: on a fresh install nothing is
// vendored yet, and the answer an operator needs is which call fixes it.
func (d *Driver) Preflight(ctx context.Context) (domain.Preflight, error) {
	pf := domain.Preflight{
		Details: map[string]string{
			"pinned_version": d.version,
			"path":           d.installer.Path(d.version),
			"config_dir":     d.configDir,
		},
	}

	installed, err := d.installer.Installed(ctx)
	if err != nil {
		return pf, err
	}
	if len(installed) > 0 {
		pf.Details["installed_versions"] = strings.Join(installed, ", ")
	}

	// usable(), not "is it listed": a directory named claude, a zero-byte file
	// or one without its execute bit all exist and none can be run.
	if !usable(d.installer.Path(d.version)) {
		pf.Blockers = append(pf.Blockers, fmt.Sprintf(
			"Claude Code %s is not installed; POST /v1/providers/%s/install", d.version, ID))
		return pf, nil
	}

	pf.Ready = true
	return pf, nil
}

func (d *Driver) AcceptedMethods() []domain.AuthMethod {
	return []domain.AuthMethod{domain.AuthManualToken}
}

// ValidateSecret checks shape only and touches no network.
func (d *Driver) ValidateSecret(method domain.AuthMethod, secret string) error {
	if method != domain.AuthManualToken {
		return fmt.Errorf("%w: %s accepts %q, not %q",
			domain.ErrCredentialInvalid, ID, domain.AuthManualToken, method)
	}

	switch {
	case secret == "":
		return fmt.Errorf("%w: no token was supplied", domain.ErrCredentialInvalid)
	case strings.TrimSpace(secret) != secret:
		return fmt.Errorf("%w: the token has leading or trailing whitespace", domain.ErrCredentialInvalid)
	case strings.ContainsAny(secret, "\n\r\t"):
		// A token is a single line. This also matters because the same value is
		// written to a PTY at the interactive step, where an embedded newline
		// would submit whatever follows it as a separate line of input.
		return fmt.Errorf("%w: the token contains a line break, so more than one line was pasted",
			domain.ErrCredentialInvalid)
	case !strings.HasPrefix(secret, tokenPrefix):
		return fmt.Errorf("%w: a Claude Code token starts with %q — `claude setup-token` prints one, "+
			"an API key (sk-ant-api…) belongs to the %s provider instead",
			domain.ErrCredentialInvalid, tokenPrefix, "anthropic-api")
	case len(secret) < minTokenLength:
		return fmt.Errorf("%w: the token is too short to be complete", domain.ErrCredentialInvalid)
	}
	return nil
}

// Materialize turns a validated token into a credential, with the best expiry it
// can establish.
func (d *Driver) Materialize(method domain.AuthMethod, secret string) (domain.Credential, error) {
	if err := d.ValidateSecret(method, secret); err != nil {
		return domain.Credential{}, err
	}

	issued := d.now()
	meta := domain.CredentialMeta{
		Hint:     hint(secret),
		Status:   string(domain.CredentialUnverified),
		IssuedAt: &issued,
	}

	if exp, ok := tokenExpiry(secret); ok {
		meta.ExpiresAt = &exp
	} else {
		// The token is opaque, so this is a guess — and it is labelled as one so
		// a client can say "about a year" rather than stating a date tumika
		// invented. Daily re-verification is what actually detects expiry.
		estimated := issued.Add(assumedTokenLifetime)
		meta.ExpiresAt = &estimated
		meta.ExpiryIsEstimate = true
	}

	return domain.Credential{
		ProviderID: ID,
		Kind:       domain.CredentialOAuthToken,
		Secret:     secret,
		Meta:       meta,
	}, nil
}

// authStatus is the shape of `claude auth status --json` that tumika reads.
//
// loggedIn is deliberately absent. It reports true for a completely bogus
// token — probed, not assumed — so reading it as a validity check would make
// verification pass for a credential that cannot do anything.
type authStatus struct {
	AuthMethod   string `json:"authMethod"`
	APIKeySource string `json:"apiKeySource"`
	Account      struct {
		Email string `json:"email"`
	} `json:"account"`
}

// promptResult is the shape of `claude -p … --output-format json`.
//
// subtype is captured only to be reported in an error message. It must never be
// the thing keyed on: a bad token returns subtype "success" ALONGSIDE
// is_error true and api_error_status 401.
type promptResult struct {
	IsError        bool   `json:"is_error"`
	APIErrorStatus int    `json:"api_error_status"`
	Subtype        string `json:"subtype"`
	Result         string `json:"result"`
}

// Verify runs the two-stage check.
//
// Stage one asks WHICH credential the CLI resolved; stage two asks whether it
// works. Neither alone is sufficient, and the order matters: a misroute to API
// billing would sail through stage two, because the API key it silently picked
// up is perfectly valid.
func (d *Driver) Verify(ctx context.Context, c domain.Credential) (domain.CredentialMeta, error) {
	meta := domain.CredentialMeta{Hint: hint(c.Secret), Status: string(domain.CredentialUnverified)}

	if c.Secret == "" {
		meta.Status = string(domain.CredentialInvalid)
		meta.LastVerifyError = "no token stored"
		return meta, nil
	}
	if !usable(d.installer.Path(d.version)) {
		// Not a verdict on the credential: there is nothing to run it with.
		return meta, fmt.Errorf("%w: Claude Code %s is not installed",
			domain.ErrProviderUnavailable, d.version)
	}

	status, err := d.authStatus(ctx, c.Secret)
	if err != nil {
		return meta, err
	}
	if status.AuthMethod != oauthAuthMethod {
		// The loud error the whole isolation policy exists to produce.
		//
		// Not ErrCredentialInvalid: the token may be perfectly good and simply
		// outranked. Reporting it as a bad credential would send the operator
		// off to mint another one, which would be outranked in exactly the same
		// way. The fault is in tumika's process construction.
		return meta, fmt.Errorf(
			"%w: the vendored claude authenticated as %q (apiKeySource %q), not %q — "+
				"tumika's subscription token was overridden, and requests would bill at API rates",
			domain.ErrProviderUnavailable, status.AuthMethod, status.APIKeySource, oauthAuthMethod)
	}
	if status.Account.Email != "" {
		meta.AccountEmail = status.Account.Email
	}

	result, err := d.prompt(ctx, c.Secret)
	if err != nil {
		return meta, err
	}

	now := d.now()

	if result.IsError {
		switch {
		case result.APIErrorStatus == 401 || result.APIErrorStatus == 403:
			// A verdict: the provider has told us the token is no good.
			meta.LastVerifiedAt = &now
			meta.Status = string(domain.CredentialInvalid)
			meta.LastVerifyError = describeResult(result)
			return meta, nil

		case result.APIErrorStatus == 429 || result.APIErrorStatus >= 500:
			// Says nothing about the token. Rate limiting in particular is the
			// EXPECTED state of a healthy subscription under load, and condemning
			// the credential for it would take a working one out of service.
			return meta, fmt.Errorf("%w: claude returned %d: %s",
				domain.ErrProviderUnavailable, result.APIErrorStatus, describeResult(result))

		default:
			// An error with no HTTP status is a local failure — a missing model,
			// a broken config — not a verdict either.
			return meta, fmt.Errorf("%w: claude reported an error: %s",
				domain.ErrProviderUnavailable, describeResult(result))
		}
	}

	meta.LastVerifiedAt = &now
	meta.Status = string(domain.CredentialActive)
	return meta, nil
}

func (d *Driver) authStatus(ctx context.Context, token string) (authStatus, error) {
	out, err := d.run(ctx, token, "auth", "status", "--json")
	if err != nil {
		return authStatus{}, err
	}

	var status authStatus
	if err := json.Unmarshal(out, &status); err != nil {
		return authStatus{}, fmt.Errorf("%w: parse `claude auth status --json`: %w",
			domain.ErrProviderUnavailable, err)
	}
	return status, nil
}

func (d *Driver) prompt(ctx context.Context, token string) (promptResult, error) {
	out, err := d.run(ctx, token, "-p", verifyPrompt, "--output-format", "json", "--max-turns", "1")

	var result promptResult
	// Parsed BEFORE the exit status is judged. A rejected token is reported in
	// the JSON body and may or may not come with a non-zero exit — so throwing
	// the output away on a failed exit would discard the verdict and report an
	// outage instead.
	if jsonErr := json.Unmarshal(out, &result); jsonErr == nil {
		return result, nil
	}
	if err != nil {
		return promptResult{}, err
	}
	return promptResult{}, fmt.Errorf("%w: `claude -p` returned output that is not JSON",
		domain.ErrProviderUnavailable)
}

// run executes one claude invocation and returns its stdout.
//
// Every error it produces is ErrProviderUnavailable: nothing this function can
// observe is a statement about the credential. A missing binary, a timeout and
// a crash all mean the check could not be carried out, and treating any of them
// as a rejection would revoke a working token over an unrelated failure.
func (d *Driver) run(ctx context.Context, token string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()

	cmd := d.command(ctx, token, args...)
	out, err := cmd.Output()
	if err == nil {
		return out, nil
	}

	// stderr can carry the provider's own explanation, and an authentication
	// endpoint is exactly the kind of thing that echoes the rejected secret
	// back. Redacted before it goes anywhere near a log or the database.
	var detail string
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
		detail = ": " + truncate(logging.Redact(strings.TrimSpace(string(exitErr.Stderr))))
	}

	if ctx.Err() != nil {
		return out, fmt.Errorf("%w: `claude %s` did not finish within %s",
			domain.ErrProviderUnavailable, args[0], d.timeout)
	}
	return out, fmt.Errorf("%w: `claude %s` failed: %w%s",
		domain.ErrProviderUnavailable, args[0], err, detail)
}

// Install downloads and verifies the pinned version.
//
// The version argument is honoured so an operator can stage a bump, but an empty
// one means the pin — which is what the endpoint sends, and what makes
// "install it" a request a client can make without knowing the number.
func (d *Driver) Install(ctx context.Context, version string) (domain.InstallResult, error) {
	if version == "" {
		version = d.version
	}
	return d.installer.Install(ctx, version)
}

func (d *Driver) Installed(ctx context.Context) ([]string, error) {
	return d.installer.Installed(ctx)
}

// Prune trims old versions, and never the pinned one.
//
// The driver supplies the protection rather than the caller, because the driver
// is what knows which version it is about to execute. A retention policy that
// could delete it would trade a full SD card for a broken install.
func (d *Driver) Prune(ctx context.Context, keep int) error {
	return d.installer.Prune(ctx, keep, d.version)
}

// tokenExpiry reads an expiry out of the token, if it happens to carry one.
//
// Subscription tokens are opaque today, so this normally returns false and the
// caller falls back to an estimate. It exists because the alternative — assuming
// opacity forever — would keep showing an invented date if the format ever
// became a JWT.
func tokenExpiry(secret string) (time.Time, bool) {
	body := strings.TrimPrefix(secret, tokenPrefix)
	parts := strings.Split(body, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, false
	}

	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Exp <= 0 {
		return time.Time{}, false
	}
	return time.Unix(claims.Exp, 0).UTC(), true
}

// describeResult renders a failed prompt for storage, bounded and redacted.
//
// It lands in provider_credentials.last_verify_error and is shown to the
// operator, so it leaves the process — which makes it credential-carrying
// surface regardless of what the CLI intended to put there.
func describeResult(r promptResult) string {
	parts := make([]string, 0, 3)
	if r.APIErrorStatus > 0 {
		parts = append(parts, fmt.Sprintf("api_error_status=%d", r.APIErrorStatus))
	}
	if r.Subtype != "" {
		// Reported, never keyed on: it says "success" for a 401.
		parts = append(parts, "subtype="+r.Subtype)
	}
	if r.Result != "" {
		parts = append(parts, r.Result)
	}
	if len(parts) == 0 {
		return "claude reported an error with no detail"
	}
	return truncate(logging.Redact(strings.Join(parts, " ")))
}

// maxDescription bounds what the CLI can push into stored metadata. A stack
// trace or an HTML error page from a proxy would otherwise put kilobytes into
// the database and hand them to a client.
const maxDescription = 256

func truncate(s string) string {
	if len(s) <= maxDescription {
		return s
	}
	// Cut on a rune boundary: slicing bytes would store invalid UTF-8 that
	// returns through JSON as replacement characters.
	cut := maxDescription
	for cut > 0 && !isRuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

// hint is the only part of a token that may be shown or stored in the clear:
// the last four characters, enough to tell two apart and useless to anyone else.
// See .agents/rules/never-log-or-return-a-credential-secret.md.
func hint(secret string) string {
	const shown = 4
	if len(secret) <= shown {
		return ""
	}
	return "…" + secret[len(secret)-shown:]
}

// Compile-time proof of exactly which capabilities this driver claims — and of
// the one it does not. The registry checks the same correspondence at startup;
// this fails at build time instead.
var (
	_ interface {
		Descriptor() domain.Descriptor
		Preflight(context.Context) (domain.Preflight, error)
		Verify(context.Context, domain.Credential) (domain.CredentialMeta, error)
		AcceptedMethods() []domain.AuthMethod
		ValidateSecret(domain.AuthMethod, string) error
		Materialize(domain.AuthMethod, string) (domain.Credential, error)
		Install(context.Context, string) (domain.InstallResult, error)
		Installed(context.Context) ([]string, error)
		Prune(context.Context, int) error
	} = (*Driver)(nil)
)
