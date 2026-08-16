package claudecode

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tumika/tumika/source/internal/domain"
	"github.com/tumika/tumika/source/internal/platform/provider"
	"github.com/tumika/tumika/source/internal/platform/provider/providertest"
)

// A token that satisfies ValidateSecret. Not a real one: the prefix is public
// and the body is filler.
const testToken = "sk-ant-oat01-0123456789abcdefghijklmnopqrstuvwxyz"

const testVersion = "2.1.233"

// fake is a stand-in `claude`, installed where the real one would be.
//
// It records its own argv and environment before answering, which is what makes
// the isolation policy testable: the guarantees in
// .agents/rules/every-spawned-claude-process-is-credential-isolated.md are all
// statements about how the child process is built, and the only way to check
// them is to ask the child.
type fake struct {
	// authStatusJSON is what `auth status --json` prints.
	authStatusJSON string
	// promptJSON is what `-p …` prints.
	promptJSON string
	// promptExit is its exit code. A rejected token may or may not come with a
	// non-zero exit, and the verdict must survive either.
	promptExit int
	// promptStderr is written to stderr before exiting.
	promptStderr string
	// authStderr is written to stderr on the auth-status path, with a non-zero
	// exit, which is the only way to reach run()'s ExitError branch.
	authStderr string
	authExit   int

	recordPath string
}

func newFake() *fake {
	return &fake{
		authStatusJSON: `{"loggedIn":true,"authMethod":"oauth_token","apiKeySource":"CLAUDE_CODE_OAUTH_TOKEN"}`,
		promptJSON:     `{"type":"result","subtype":"success","is_error":false,"result":"ok"}`,
	}
}

// install writes the fake to <providers>/claude-code/<version>/claude and
// returns a driver pointed at it.
func (f *fake) install(t *testing.T, opts ...DriverOption) *Driver {
	t.Helper()

	home := t.TempDir()
	providers := filepath.Join(home, "providers")
	configDir := filepath.Join(home, "claude")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatalf("mkdir config: %v", err)
	}

	f.recordPath = filepath.Join(home, "invocation.txt")

	dir := filepath.Join(providers, "claude-code", testVersion)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	script := fmt.Sprintf(`#!/bin/sh
{
  echo "ARGV_BEGIN"
  for a in "$@"; do echo "arg:$a"; done
  echo "ARGV_END"
  echo "ENV_BEGIN"
  env
  echo "ENV_END"
  echo "CWD:$PWD"
} >> %q

case "$3" in
  auth)
    printf '%%s' %q >&2
    cat <<'JSON'
%s
JSON
    exit %d
    ;;
  -p)
    printf '%%s' %q >&2
    cat <<'JSON'
%s
JSON
    exit %d
    ;;
esac
`, f.recordPath, f.authStderr, f.authStatusJSON, f.authExit, f.promptStderr, f.promptJSON, f.promptExit)

	if err := os.WriteFile(filepath.Join(dir, BinaryName), []byte(script), 0o700); err != nil {
		t.Fatalf("write fake: %v", err)
	}

	opts = append([]DriverOption{WithVerifyTimeout(30 * time.Second)}, opts...)
	d, err := NewDriver(providers, configDir, testVersion, opts...)
	if err != nil {
		t.Fatalf("NewDriver: %v", err)
	}
	return d
}

// argv returns the arguments of the first recorded invocation, including the
// empty ones. That matters: the settings-sources flag is passed an EMPTY
// argument, and a naive split would lose it.
func (f *fake) argv(t *testing.T) []string {
	t.Helper()

	var args []string
	in := false
	for _, line := range strings.Split(f.first(t), "\n") {
		switch {
		case line == "ARGV_BEGIN":
			in = true
		case line == "ARGV_END":
			return args
		case in && strings.HasPrefix(line, "arg:"):
			args = append(args, strings.TrimPrefix(line, "arg:"))
		}
	}
	return args
}

// env returns the environment of the first recorded invocation.
func (f *fake) env(t *testing.T) map[string]string {
	t.Helper()

	out := map[string]string{}
	in := false
	for _, line := range strings.Split(f.first(t), "\n") {
		switch {
		case line == "ENV_BEGIN":
			in = true
		case line == "ENV_END":
			return out
		case in:
			if name, value, ok := strings.Cut(line, "="); ok {
				out[name] = value
			}
		}
	}
	return out
}

func (f *fake) first(t *testing.T) string {
	t.Helper()

	raw, err := os.ReadFile(f.recordPath)
	if err != nil {
		t.Fatalf("the fake claude was never invoked: %v", err)
	}
	return string(raw)
}

// THE regression test the isolation rule names.
//
// The daemon's OWN environment is poisoned with every variable that outranks
// CLAUDE_CODE_OAUTH_TOKEN, and the child must see none of them. This is the
// failure with no symptom: everything works, the answers come back correct, and
// the operator finds out from an API invoice weeks later that the subscription
// they installed tumika to use was never touched.
func TestASpawnedClaudeInheritsNoCredentialPrecedenceVariable(t *testing.T) {
	poison := map[string]string{
		"ANTHROPIC_API_KEY":              "sk-ant-api-should-not-reach-the-child",
		"ANTHROPIC_AUTH_TOKEN":           "should-not-reach-the-child",
		"ANTHROPIC_BASE_URL":             "https://not-anthropic.example",
		"ANTHROPIC_PROFILE":              "someone-elses-profile",
		"CLAUDE_CODE_USE_BEDROCK":        "1",
		"CLAUDE_CODE_USE_VERTEX":         "1",
		"CLAUDE_CODE_USE_FOUNDRY":        "1",
		"AWS_ACCESS_KEY_ID":              "AKIAEXAMPLE",
		"AWS_BEARER_TOKEN_BEDROCK":       "bedrock",
		"GOOGLE_APPLICATION_CREDENTIALS": "/tmp/creds.json",
		// Reroutes billing outright by injecting an Authorization header,
		// without touching any of the variables above.
		"ANTHROPIC_CUSTOM_HEADERS":   "Authorization: Bearer someone-elses-key",
		"ANTHROPIC_BEDROCK_BASE_URL": "https://bedrock.example",
		"ANTHROPIC_VERTEX_BASE_URL":  "https://vertex.example",
	}
	for name, value := range poison {
		t.Setenv(name, value)
	}

	f := newFake()
	d := f.install(t)

	meta, err := d.Verify(t.Context(), domain.Credential{ProviderID: ID, Secret: testToken})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if meta.Status != string(domain.CredentialActive) {
		t.Fatalf("status = %q, want active", meta.Status)
	}

	childEnv := f.env(t)
	for name := range poison {
		if got, present := childEnv[name]; present {
			t.Errorf("the child inherited %s=%q; requests would bill at API rates", name, got)
		}
	}
	if childEnv["CLAUDE_CODE_OAUTH_TOKEN"] != testToken {
		t.Error("the child did not receive tumika's subscription token")
	}
	if childEnv["DISABLE_AUTOUPDATER"] != "1" {
		t.Error("the auto-updater is live, so the pinned version can move underneath the parser")
	}
	if childEnv["CLAUDE_CONFIG_DIR"] == "" || childEnv["CLAUDE_CONFIG_DIR"] != d.configDir {
		t.Errorf("CLAUDE_CONFIG_DIR = %q, want tumika's isolated directory %q",
			childEnv["CLAUDE_CONFIG_DIR"], d.configDir)
	}
	if childEnv["HOME"] == os.Getenv("HOME") {
		t.Error("the child inherited the operator's HOME, so it can read their own Claude configuration")
	}
}

// Passing the settings-sources flag an empty argument is the measure that is
// easy to omit and impossible to notice: apiKeyHelper is a settings-file key, invisible to `env`, and it
// outranks the token tumika injects. Only refusing to load settings closes it.
func TestEverySpawnedClaudeRefusesToLoadSettings(t *testing.T) {
	f := newFake()
	d := f.install(t)

	if _, err := d.Verify(t.Context(), domain.Credential{ProviderID: ID, Secret: testToken}); err != nil {
		t.Fatalf("Verify: %v", err)
	}

	args := f.argv(t)
	if len(args) < 2 || args[0] != "--setting-sources" || args[1] != "" {
		t.Fatalf("argv = %q, want it to begin with --setting-sources ''", args)
	}
	for _, arg := range args {
		if arg == "--bare" {
			t.Error("--bare does not read CLAUDE_CODE_OAUTH_TOKEN and falls through the precedence chain")
		}
	}
}

// The binary is executed by absolute path, at the pinned version. No PATH lookup
// to win, and no launcher symlink to repoint.
func TestTheVendoredBinaryIsRunByAbsolutePath(t *testing.T) {
	f := newFake()
	d := f.install(t)

	cmd := d.command(t.Context(), testToken, "auth", "status", "--json")
	if !filepath.IsAbs(cmd.Path) {
		t.Errorf("cmd.Path = %q, want an absolute path", cmd.Path)
	}
	if !strings.Contains(cmd.Path, filepath.Join("claude-code", testVersion)) {
		t.Errorf("cmd.Path = %q, want the pinned version's directory", cmd.Path)
	}
}

// Stage one is the whole point of the two-stage check: a misroute to API billing
// would sail through stage two, because the key it silently picked up is valid.
func TestVerifyRefusesAnythingButTheSubscriptionToken(t *testing.T) {
	for _, method := range []string{"api_key", "bedrock", "vertex", ""} {
		f := newFake()
		f.authStatusJSON = fmt.Sprintf(
			`{"loggedIn":true,"authMethod":%q,"apiKeySource":"ANTHROPIC_API_KEY"}`, method)
		d := f.install(t)

		meta, err := d.Verify(t.Context(), domain.Credential{ProviderID: ID, Secret: testToken})
		if err == nil {
			t.Fatalf("authMethod %q was accepted: %+v", method, meta)
		}
		// NOT a bad credential: the token may be perfectly good and simply
		// outranked, and telling the operator to mint another would produce one
		// that is outranked in exactly the same way.
		if !errors.Is(err, domain.ErrProviderUnavailable) {
			t.Errorf("authMethod %q gave %v, want ErrProviderUnavailable", method, err)
		}
		if errors.Is(err, domain.ErrCredentialInvalid) {
			t.Errorf("authMethod %q was blamed on the credential", method)
		}
		if !strings.Contains(err.Error(), "API rates") {
			t.Errorf("the error should say what it costs, got: %v", err)
		}
	}
}

// `claude auth status --json` reports loggedIn: true for a completely bogus
// token — probed, not assumed. Reading it would make verification pass for a
// credential that cannot do anything at all.
func TestVerifyDoesNotTrustLoggedIn(t *testing.T) {
	f := newFake()
	f.authStatusJSON = `{"loggedIn":true,"authMethod":"oauth_token"}`
	f.promptJSON = `{"type":"result","subtype":"success","is_error":true,"api_error_status":401,` +
		`"result":"OAuth authentication is currently not supported."}`
	d := f.install(t)

	meta, err := d.Verify(t.Context(), domain.Credential{ProviderID: ID, Secret: testToken})
	if err != nil {
		t.Fatalf("a rejection is a verdict, not an error: %v", err)
	}
	if meta.Status != string(domain.CredentialInvalid) {
		t.Fatalf("status = %q, want invalid despite loggedIn: true", meta.Status)
	}
	if meta.LastVerifiedAt == nil {
		t.Error("a verdict was reached, so it happened at a time worth recording")
	}
}

// The second probed fact: `claude -p` with a bad token returns
// subtype "success" ALONGSIDE is_error true and api_error_status 401. Keying on
// subtype — the obvious field — passes on an authentication failure.
func TestVerifyKeysOnIsErrorRatherThanSubtype(t *testing.T) {
	f := newFake()
	f.promptJSON = `{"type":"result","subtype":"success","is_error":true,"api_error_status":401,"result":"denied"}`
	f.promptExit = 1
	d := f.install(t)

	meta, err := d.Verify(t.Context(), domain.Credential{ProviderID: ID, Secret: testToken})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if meta.Status != string(domain.CredentialInvalid) {
		t.Fatalf("status = %q, want invalid — subtype said success", meta.Status)
	}
	if !strings.Contains(meta.LastVerifyError, "api_error_status=401") {
		t.Errorf("last_verify_error = %q, want the status that explains it", meta.LastVerifyError)
	}
}

// Rate limiting is the EXPECTED state of a healthy subscription under load.
// Condemning the credential for it would take a working one out of service, and
// the daily monitor would keep doing so.
func TestATransientFailureIsNotAVerdict(t *testing.T) {
	for _, status := range []int{429, 500, 503, 529} {
		f := newFake()
		f.promptJSON = fmt.Sprintf(
			`{"subtype":"error","is_error":true,"api_error_status":%d,"result":"slow down"}`, status)
		d := f.install(t)

		meta, err := d.Verify(t.Context(), domain.Credential{ProviderID: ID, Secret: testToken})
		if !errors.Is(err, domain.ErrProviderUnavailable) {
			t.Errorf("%d gave %v, want ErrProviderUnavailable", status, err)
		}
		if meta.Status == string(domain.CredentialInvalid) {
			t.Errorf("%d condemned the credential", status)
		}
	}
}

// A local failure — a missing model, a broken config — carries no HTTP status
// and says nothing about the token either.
func TestAnErrorWithoutAnHTTPStatusIsNotAVerdict(t *testing.T) {
	f := newFake()
	f.promptJSON = `{"subtype":"error_during_execution","is_error":true,"result":"model not found"}`
	d := f.install(t)

	meta, err := d.Verify(t.Context(), domain.Credential{ProviderID: ID, Secret: testToken})
	if !errors.Is(err, domain.ErrProviderUnavailable) {
		t.Fatalf("= %v, want ErrProviderUnavailable", err)
	}
	if meta.Status == string(domain.CredentialInvalid) {
		t.Error("a local failure condemned the credential")
	}
}

// A rejected token may or may not come with a non-zero exit. Throwing the output
// away on a failed exit would discard the verdict and report an outage instead —
// so the daily monitor would never mark an expired token expired.
func TestAVerdictSurvivesANonZeroExit(t *testing.T) {
	f := newFake()
	f.promptJSON = `{"subtype":"success","is_error":true,"api_error_status":401,"result":"expired"}`
	f.promptExit = 2
	f.promptStderr = "claude: authentication failed"
	d := f.install(t)

	meta, err := d.Verify(t.Context(), domain.Credential{ProviderID: ID, Secret: testToken})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if meta.Status != string(domain.CredentialInvalid) {
		t.Errorf("status = %q, want invalid", meta.Status)
	}
}

// Output that is not JSON at all cannot be read as a verdict in either
// direction.
func TestUnparseableOutputIsNotAVerdict(t *testing.T) {
	f := newFake()
	f.promptJSON = `<html>502 Bad Gateway</html>`
	d := f.install(t)

	meta, err := d.Verify(t.Context(), domain.Credential{ProviderID: ID, Secret: testToken})
	if !errors.Is(err, domain.ErrProviderUnavailable) {
		t.Fatalf("= %v, want ErrProviderUnavailable", err)
	}
	if meta.Status == string(domain.CredentialActive) {
		t.Error("unreadable output was reported as a working credential")
	}
}

// Nothing to run the credential with is not a statement about the credential.
func TestVerifyWithoutAnInstalledBinary(t *testing.T) {
	home := t.TempDir()
	d, err := NewDriver(filepath.Join(home, "providers"), filepath.Join(home, "claude"), testVersion)
	if err != nil {
		t.Fatalf("NewDriver: %v", err)
	}

	meta, err := d.Verify(t.Context(), domain.Credential{ProviderID: ID, Secret: testToken})
	if !errors.Is(err, domain.ErrProviderUnavailable) {
		t.Fatalf("= %v, want ErrProviderUnavailable", err)
	}
	if meta.Status == string(domain.CredentialInvalid) {
		t.Error("a missing binary was blamed on the credential")
	}
}

// An empty secret is a verdict: there is nothing to check and nothing to blame
// on the provider.
func TestVerifyWithNoStoredToken(t *testing.T) {
	f := newFake()
	d := f.install(t)

	meta, err := d.Verify(t.Context(), domain.Credential{ProviderID: ID})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if meta.Status != string(domain.CredentialInvalid) {
		t.Errorf("status = %q, want invalid", meta.Status)
	}
}

// stderr from an authentication endpoint is exactly the kind of thing that
// echoes the rejected secret back — and it reaches an operator's terminal and
// the daemon's log.
//
// The earlier version of this test set authStatusJSON to rubbish, which failed
// at json.Unmarshal: a path whose error text is a parse error and structurally
// cannot contain a token. It could not fail, and deleting the Redact call left
// it green. This one drives the CLI to exit non-zero with the token on stderr,
// which is the branch that actually does the redacting.
func TestStderrFromAFailedInvocationIsRedacted(t *testing.T) {
	f := newFake()
	f.authStderr = "Error: rejected credential " + testToken + " (expired)"
	f.authExit = 1
	d := f.install(t)

	_, err := d.Verify(t.Context(), domain.Credential{ProviderID: ID, Secret: testToken})
	if err == nil {
		t.Fatal("a failed `claude auth status` was accepted")
	}
	if strings.Contains(err.Error(), testToken) {
		t.Errorf("the error carries the token: %v", err)
	}
	// The explanation itself must survive; redaction that discarded the message
	// would make every failure look identical.
	if !strings.Contains(err.Error(), "expired") {
		t.Errorf("the provider's explanation was lost: %v", err)
	}
}

// The same surface, on the path that reaches the DATABASE: last_verify_error is
// stored and handed back to a client.
func TestAStoredVerifyErrorIsRedacted(t *testing.T) {
	f := newFake()
	f.promptJSON = `{"type":"result","subtype":"success","is_error":true,"api_error_status":401,` +
		`"result":"token ` + testToken + ` was revoked"}`
	d := f.install(t)

	meta, err := d.Verify(t.Context(), domain.Credential{ProviderID: ID, Secret: testToken})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if meta.Status != string(domain.CredentialInvalid) {
		t.Fatalf("status = %q, want invalid", meta.Status)
	}
	if strings.Contains(meta.LastVerifyError, testToken) {
		t.Errorf("the stored error carries the token: %q", meta.LastVerifyError)
	}
	if !strings.Contains(meta.LastVerifyError, "revoked") {
		t.Errorf("the reason was lost: %q", meta.LastVerifyError)
	}
}

// Every field of a result is optional to a JSON decoder, so `null`, `{}` and an
// error envelope all decode into a zero value — and a zero value has is_error
// false. Reading that as success reported a credential that produced no answer
// at all as ACTIVE: the same mistake as keying on subtype, reached from the
// other side.
func TestADocumentThatIsNotAResultIsNotSuccess(t *testing.T) {
	for _, body := range []string{
		`null`,
		`{}`,
		`{"type":"error","error":{"message":"boom"}}`,
		`{"is_error":false}`,
	} {
		f := newFake()
		f.promptJSON = body
		d := f.install(t)

		meta, err := d.Verify(t.Context(), domain.Credential{ProviderID: ID, Secret: testToken})
		if err == nil {
			t.Errorf("%s was accepted as a working credential (status %q)", body, meta.Status)
			continue
		}
		if !errors.Is(err, domain.ErrProviderUnavailable) {
			t.Errorf("%s gave %v, want ErrProviderUnavailable", body, err)
		}
		if meta.Status == string(domain.CredentialActive) {
			t.Errorf("%s was recorded as active", body)
		}
	}
}

// A real result is still accepted, so the check above cannot pass by refusing
// everything.
func TestARealResultIsStillAccepted(t *testing.T) {
	f := newFake()
	d := f.install(t)

	meta, err := d.Verify(t.Context(), domain.Credential{ProviderID: ID, Secret: testToken})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if meta.Status != string(domain.CredentialActive) {
		t.Fatalf("status = %q, want active", meta.Status)
	}
}

func TestValidateSecret(t *testing.T) {
	f := newFake()
	d := f.install(t)

	valid := []string{testToken}
	invalid := map[string]string{
		"empty":         "",
		"an API key":    "sk-ant-api03-0123456789abcdefghijklmnopqrstuvwxyz",
		"truncated":     "sk-ant-oat01-short",
		"leading space": " " + testToken,
		"trailing tab":  testToken + "\t",
		// A pasted terminal selection can carry the next line with it, and the
		// same value is later written to a PTY.
		"two lines": testToken + "\nsomething else",
	}

	for _, secret := range valid {
		if err := d.ValidateSecret(domain.AuthManualToken, secret); err != nil {
			t.Errorf("a valid token was rejected: %v", err)
		}
	}
	for name, secret := range invalid {
		err := d.ValidateSecret(domain.AuthManualToken, secret)
		if err == nil {
			t.Errorf("%s was accepted", name)
			continue
		}
		if !errors.Is(err, domain.ErrCredentialInvalid) {
			t.Errorf("%s gave %v, want ErrCredentialInvalid", name, err)
		}
		if strings.Contains(err.Error(), secret) && secret != "" {
			t.Errorf("%s: the error echoes the secret: %v", name, err)
		}
	}

	if err := d.ValidateSecret(domain.AuthAPIKey, testToken); err == nil {
		t.Error("the wrong auth method was accepted")
	}
}

// An API key pasted into the wrong provider is the likely mistake, so the
// refusal says where it belongs rather than only that it is wrong.
func TestAnAPIKeyIsRedirectedToTheRightProvider(t *testing.T) {
	f := newFake()
	d := f.install(t)

	err := d.ValidateSecret(domain.AuthManualToken, "sk-ant-api03-0123456789abcdefghijklmnopqrst")
	if err == nil {
		t.Fatal("an API key was accepted")
	}
	if !strings.Contains(err.Error(), "anthropic-api") {
		t.Errorf("the error should name the provider that takes it, got: %v", err)
	}
}

func TestMaterializeEstimatesExpiry(t *testing.T) {
	issued := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)

	f := newFake()
	d := f.install(t, WithClock(func() time.Time { return issued }))

	cred, err := d.Materialize(domain.AuthManualToken, testToken)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}

	if cred.Kind != domain.CredentialOAuthToken {
		t.Errorf("kind = %q, want %q", cred.Kind, domain.CredentialOAuthToken)
	}
	if cred.ProviderID != ID {
		t.Errorf("provider = %q, want %q", cred.ProviderID, ID)
	}
	if cred.Secret != testToken {
		t.Error("the secret did not survive materialisation")
	}
	if cred.Meta.Status != string(domain.CredentialUnverified) {
		t.Errorf("status = %q, want unverified — nothing has been proven yet", cred.Meta.Status)
	}
	if cred.Meta.Hint != "…wxyz" {
		t.Errorf("hint = %q, want the last four characters only", cred.Meta.Hint)
	}
	if strings.Contains(cred.Meta.Hint, testToken) {
		t.Error("the hint carries the token")
	}

	// The token is opaque, so the date is invented — and must say so, or a
	// client will present tumika's guess as Anthropic's answer.
	if !cred.Meta.ExpiryIsEstimate {
		t.Error("an assumed expiry was not flagged as an estimate")
	}
	if cred.Meta.ExpiresAt == nil || !cred.Meta.ExpiresAt.Equal(issued.Add(assumedTokenLifetime)) {
		t.Errorf("expires_at = %v, want issued + a year", cred.Meta.ExpiresAt)
	}
	if cred.Meta.IssuedAt == nil || !cred.Meta.IssuedAt.Equal(issued) {
		t.Errorf("issued_at = %v, want %v", cred.Meta.IssuedAt, issued)
	}
}

// If the format ever becomes a JWT, the real expiry is used and is no longer
// flagged as a guess. Assuming opacity forever would keep showing an invented
// date after the day it stopped being true.
func TestMaterializeReadsARealExpiryWhenThereIsOne(t *testing.T) {
	f := newFake()
	d := f.install(t)

	exp := time.Date(2027, 1, 2, 3, 4, 5, 0, time.UTC)
	token := tokenPrefix + jwt(t, exp)

	cred, err := d.Materialize(domain.AuthManualToken, token)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	if cred.Meta.ExpiryIsEstimate {
		t.Error("a stated expiry was flagged as an estimate")
	}
	if cred.Meta.ExpiresAt == nil || !cred.Meta.ExpiresAt.Equal(exp) {
		t.Errorf("expires_at = %v, want the token's own %v", cred.Meta.ExpiresAt, exp)
	}
}

func TestPreflightSaysWhatToDoAboutAMissingBinary(t *testing.T) {
	home := t.TempDir()
	d, err := NewDriver(filepath.Join(home, "providers"), filepath.Join(home, "claude"), testVersion)
	if err != nil {
		t.Fatalf("NewDriver: %v", err)
	}

	pf, err := d.Preflight(t.Context())
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if pf.Ready {
		t.Fatal("preflight is ready with nothing installed")
	}
	if len(pf.Blockers) == 0 {
		t.Fatal("not ready, and no blocker says why")
	}
	if !strings.Contains(pf.Blockers[0], "install") {
		t.Errorf("the blocker should name the next step, got: %q", pf.Blockers[0])
	}
	if pf.Details["pinned_version"] != testVersion {
		t.Errorf("details omit the pinned version: %v", pf.Details)
	}
}

func TestPreflightIsReadyOnceInstalled(t *testing.T) {
	f := newFake()
	d := f.install(t)

	pf, err := d.Preflight(t.Context())
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if !pf.Ready {
		t.Fatalf("preflight is not ready with the pinned version installed: %+v", pf)
	}
	if len(pf.Blockers) != 0 {
		t.Errorf("ready, but reports blockers: %v", pf.Blockers)
	}
}

// A directory named `claude`, or one with the execute bit stripped, satisfies
// os.Stat and cannot be run. Preflight has to agree with what Verify will do.
func TestPreflightIsNotReadyForAnUnrunnableBinary(t *testing.T) {
	f := newFake()
	d := f.install(t)

	binary := d.installer.Path(testVersion)
	if err := os.Chmod(binary, 0o600); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	pf, err := d.Preflight(t.Context())
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if pf.Ready {
		t.Error("preflight is ready for a binary that cannot be executed")
	}
}

// The suite every driver must pass, written once and run against each of them,
// so a second driver cannot quietly diverge from the first.
func TestConformance(t *testing.T) {
	f := newFake()
	providertest.Conformance(t, f.install(t))
}

// The registry validates the descriptor against the interfaces the driver
// actually implements, so this is where a lie about capabilities is caught.
func TestTheDriverSatisfiesTheRegistry(t *testing.T) {
	f := newFake()
	d := f.install(t)

	reg, err := provider.NewRegistry(d)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	if _, err := reg.StaticAuthenticator(ID); err != nil {
		t.Errorf("the manual_token path is not reachable: %v", err)
	}
	if _, err := reg.Installer(ID); err != nil {
		t.Errorf("a managed provider offers no installer: %v", err)
	}
	if _, err := reg.HealthChecker(ID); err != nil {
		t.Errorf("no health checker: %v", err)
	}

	// The PTY login does not exist yet, and its absence is DISCOVERABLE: the
	// registry answers with a sentinel rather than a nil driver, which is what
	// the login endpoint will encode as a 400 when it is added. No such route
	// exists today.
	if _, err := reg.InteractiveAuthenticator(ID); !errors.Is(err, domain.ErrInteractiveAuthUnsupported) {
		t.Errorf("= %v, want ErrInteractiveAuthUnsupported until the PTY flow lands", err)
	}
	if d.Descriptor().RequiresInteractiveAuth() {
		t.Error("the descriptor promises an interactive login that does not exist yet")
	}
}

// A driver with no config directory would fall back to ~/.claude and read — or
// rewrite — the operator's own Claude Code configuration.
func TestADriverWithoutAnIsolatedConfigDirectory(t *testing.T) {
	if _, err := NewDriver(t.TempDir(), "", testVersion); err == nil {
		t.Fatal("a driver with no CLAUDE_CONFIG_DIR was built")
	}
}

// The pin is interpolated into a filesystem path and a URL, so it goes through
// the same refusal as any other version.
func TestADriverWithATraversingPin(t *testing.T) {
	if _, err := NewDriver(t.TempDir(), t.TempDir(), "../../../usr/local/bin"); !errors.Is(err, ErrInvalidVersion) {
		t.Fatalf("= %v, want ErrInvalidVersion", err)
	}
}

// Prune protects the pinned version without being told to. The caller knows a
// retention count; only the driver knows which version it is about to execute.
func TestPruneNeverRemovesThePinnedVersion(t *testing.T) {
	f := newFake()
	d := f.install(t)

	// Two newer versions, so a naive "keep the newest 1" would take the pin.
	for _, v := range []string{"2.1.240", "2.1.250"} {
		dir := filepath.Join(d.installer.root, v)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, BinaryName), []byte("#!/bin/sh\n"), 0o700); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	if err := d.Prune(t.Context(), 1); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	installed, err := d.Installed(t.Context())
	if err != nil {
		t.Fatalf("Installed: %v", err)
	}
	if !contains(installed, testVersion) {
		t.Errorf("Prune removed the version the driver is about to run: %v", installed)
	}
}

// An empty version means the pin, so a client can ask for "install it" without
// knowing the number — which is the point of the number living in exactly one
// place.
func TestInstallDefaultsToThePin(t *testing.T) {
	f := newFake()
	d := f.install(t)

	result, err := d.Install(t.Context(), "")
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if result.Version != testVersion {
		t.Errorf("version = %q, want the pinned %q", result.Version, testVersion)
	}
	if !result.AlreadyPresent {
		t.Error("an installed version was downloaded again")
	}
}

// scrub is redundant against the fixed environment above — and exists for the
// day someone adds a passthrough. Testing it directly is the only way to say so.
//
// The variables are named HERE rather than read from deniedEnv, because a test
// that iterates the list it is checking passes for any list, including an empty
// one. Every entry below is on the credential precedence chain ahead of
// CLAUDE_CODE_OAUTH_TOKEN, so each is a way to bill the operator at API rates.
func TestScrubDropsEveryOverridingVariable(t *testing.T) {
	overriding := []string{
		"ANTHROPIC_API_KEY",
		"ANTHROPIC_AUTH_TOKEN",
		"ANTHROPIC_BASE_URL",
		"ANTHROPIC_PROFILE",
		"ANTHROPIC_MODEL",
		// Carries an Authorization header, so it reroutes billing without
		// touching any of the above.
		"ANTHROPIC_CUSTOM_HEADERS",
		"ANTHROPIC_BEDROCK_BASE_URL",
		"ANTHROPIC_VERTEX_BASE_URL",
		"CLAUDE_CODE_USE_BEDROCK",
		"CLAUDE_CODE_USE_VERTEX",
		"CLAUDE_CODE_USE_FOUNDRY",
		"AWS_ACCESS_KEY_ID",
		"AWS_SECRET_ACCESS_KEY",
		"AWS_SESSION_TOKEN",
		"AWS_PROFILE",
		"AWS_REGION",
		"AWS_BEARER_TOKEN_BEDROCK",
		"GOOGLE_APPLICATION_CREDENTIALS",
		"GOOGLE_CLOUD_PROJECT",
		"CLOUD_ML_REGION",
		"AZURE_OPENAI_API_KEY",
	}

	env := []string{"PATH=/usr/bin", "HOME=/home/tumika", "CLAUDE_CODE_OAUTH_TOKEN=" + testToken}
	for _, name := range overriding {
		env = append(env, name+"=would-override-tumikas-token")
	}

	kept := map[string]bool{}
	for _, kv := range scrub(env) {
		name, _, _ := strings.Cut(kv, "=")
		kept[name] = true
	}

	for _, name := range overriding {
		if kept[name] {
			t.Errorf("scrub kept %s; a claude given it would bill at API rates", name)
		}
	}
	if !kept["CLAUDE_CODE_OAUTH_TOKEN"] {
		t.Error("scrub dropped tumika's own token")
	}
	if !kept["PATH"] || !kept["HOME"] {
		t.Error("scrub dropped a variable the child needs")
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// jwt builds a token body that carries an exp claim.
func jwt(t *testing.T, exp time.Time) string {
	t.Helper()

	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none"}`)) + "." +
		enc([]byte(fmt.Sprintf(`{"exp":%d}`, exp.Unix()))) + "." +
		enc([]byte("sig"))
}

// Claude Code is a Node program that spawns children, and exec.CommandContext
// signals only the process it started. On a daemon, a timeout that leaves those
// children behind accumulates them for as long as it is up.
func TestATimeoutKillsTheWholeProcessGroup(t *testing.T) {
	home := t.TempDir()
	providers := filepath.Join(home, "providers")
	configDir := filepath.Join(home, "claude")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	dir := filepath.Join(providers, "claude-code", testVersion)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	marker := filepath.Join(home, "child-still-running")
	// A background child that outlives its parent, and keeps proving it. If the
	// group is not killed, the marker keeps moving after the timeout.
	script := fmt.Sprintf(`#!/bin/sh
(while true; do date +%%s > %q; sleep 0.05; done) &
sleep 30
`, marker)
	if err := os.WriteFile(filepath.Join(dir, BinaryName), []byte(script), 0o700); err != nil {
		t.Fatalf("write: %v", err)
	}

	d, err := NewDriver(providers, configDir, testVersion, WithVerifyTimeout(500*time.Millisecond))
	if err != nil {
		t.Fatalf("NewDriver: %v", err)
	}

	start := time.Now()
	_, err = d.Verify(t.Context(), domain.Credential{ProviderID: ID, Secret: testToken})
	if err == nil {
		t.Fatal("a hung claude was reported as a working credential")
	}
	if !errors.Is(err, domain.ErrProviderUnavailable) {
		t.Errorf("= %v, want ErrProviderUnavailable", err)
	}
	if elapsed := time.Since(start); elapsed > 15*time.Second {
		t.Fatalf("Verify took %s; the timeout did not take effect", elapsed)
	}

	// Give the orphan a chance to keep writing, then check it stopped.
	first, err := os.ReadFile(marker)
	if err != nil {
		t.Skipf("the background child never started, so there is nothing to prove: %v", err)
	}
	time.Sleep(500 * time.Millisecond)
	second, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("read the marker: %v", err)
	}
	if string(first) != string(second) {
		t.Error("a grandchild survived the timeout; on a daemon these accumulate")
	}
}

// A caller going away is not the CLI hanging.
//
// Both contexts are cancelled when the deadline passes, so reading only the
// derived one reported an HTTP client disconnect or a daemon shutdown as
// "did not finish within 60s" — sending the operator to look for a stall that
// never happened.
func TestACancelledCallerIsNotReportedAsAHang(t *testing.T) {
	home := t.TempDir()
	providers := filepath.Join(home, "providers")
	configDir := filepath.Join(home, "claude")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	dir := filepath.Join(providers, "claude-code", testVersion)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, BinaryName), []byte("#!/bin/sh\nsleep 30\n"), 0o700); err != nil {
		t.Fatalf("write: %v", err)
	}

	// A verify timeout far longer than the test, so anything that finishes
	// quickly finished because the CALLER stopped.
	d, err := NewDriver(providers, configDir, testVersion, WithVerifyTimeout(10*time.Minute))
	if err != nil {
		t.Fatalf("NewDriver: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err = d.Verify(ctx, domain.Credential{ProviderID: ID, Secret: testToken})
	if err == nil {
		t.Fatal("a cancelled verify reported success")
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("cancellation took %s to take effect", elapsed)
	}
	if !errors.Is(err, domain.ErrProviderUnavailable) {
		t.Errorf("= %v, want ErrProviderUnavailable", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("= %v, want it to name the cancellation", err)
	}
	if strings.Contains(err.Error(), "did not finish within") {
		t.Errorf("a cancelled caller was reported as a hung CLI: %v", err)
	}
}
