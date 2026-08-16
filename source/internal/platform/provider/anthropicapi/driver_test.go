package anthropicapi_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/tumika/tumika/source/internal/domain"
	"github.com/tumika/tumika/source/internal/platform/provider/anthropicapi"
	"github.com/tumika/tumika/source/internal/platform/provider/providertest"
)

const testKey = "sk-ant-api03-" + "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

// The shared suite, run against this driver. Writing it once and running it
// against every implementation is what stops a second driver quietly diverging
// from the first.
func TestConformance(t *testing.T) {
	providertest.Conformance(t, anthropicapi.New())
}

func TestDescriptorDeclaresOnlyWhatItImplements(t *testing.T) {
	d := anthropicapi.New().Descriptor()

	if d.RequiresInteractiveAuth() {
		t.Error("an API key needs no login session")
	}
	if d.Managed {
		t.Error("Managed = true, but this provider installs nothing")
	}
	if d.Kind != domain.ProviderKindHTTP {
		t.Errorf("Kind = %q", d.Kind)
	}
}

func TestValidateSecret(t *testing.T) {
	driver := anthropicapi.New()

	tests := map[string]struct {
		method  domain.AuthMethod
		secret  string
		wantErr bool
	}{
		"a well-formed key":         {domain.AuthAPIKey, testKey, false},
		"empty":                     {domain.AuthAPIKey, "", true},
		"wrong prefix":              {domain.AuthAPIKey, "api-key-12345678901234567890", true},
		"too short":                 {domain.AuthAPIKey, "sk-ant-x", true},
		"leading whitespace":        {domain.AuthAPIKey, " " + testKey, true},
		"trailing newline":          {domain.AuthAPIKey, testKey + "\n", true},
		"an oauth token, not a key": {domain.AuthManualToken, testKey, true},
		"an unknown method":         {domain.AuthMethod("telepathy"), testKey, true},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			err := driver.ValidateSecret(tc.method, tc.secret)
			if tc.wantErr && err == nil {
				t.Error("accepted a secret it should have rejected")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("rejected a valid key: %v", err)
			}
		})
	}
}

func TestMaterializeCarriesAHintAndNoMore(t *testing.T) {
	cred, err := anthropicapi.New().Materialize(domain.AuthAPIKey, testKey)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}

	if cred.Kind != domain.CredentialAPIKey {
		t.Errorf("Kind = %q", cred.Kind)
	}
	if cred.Secret != testKey {
		t.Error("the secret was altered")
	}
	if cred.Meta.Status != string(domain.CredentialUnverified) {
		t.Errorf("Status = %q; a freshly submitted key has not been verified yet", cred.Meta.Status)
	}
	// The hint is the only part that may be shown. It must not be enough to
	// reconstruct the key.
	if strings.Contains(cred.Meta.Hint, "sk-ant") || utf8.RuneCountInString(cred.Meta.Hint) > 5 {
		t.Errorf("Hint = %q reveals too much", cred.Meta.Hint)
	}
}

// verifyAgainst stands up a fake Anthropic API.
func verifyAgainst(t *testing.T, handler http.HandlerFunc) (domain.CredentialMeta, error) {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	driver := anthropicapi.New(anthropicapi.WithBaseURL(server.URL), anthropicapi.WithHTTPClient(server.Client()))
	return driver.Verify(t.Context(), domain.Credential{
		ProviderID: anthropicapi.ID,
		Kind:       domain.CredentialAPIKey,
		Secret:     testKey,
	})
}

func TestVerifyAcceptsAWorkingKey(t *testing.T) {
	meta, err := verifyAgainst(t, func(w http.ResponseWriter, r *http.Request) {
		// The API refuses a request without its version header, so sending it is
		// part of the contract, not decoration.
		if r.Header.Get("anthropic-version") == "" {
			t.Error("the anthropic-version header was not sent")
		}
		if r.Header.Get("x-api-key") != testKey {
			t.Errorf("x-api-key = %q", r.Header.Get("x-api-key"))
		}
		if r.URL.Path != "/v1/models" {
			t.Errorf("verification hit %q; it should use the cheapest authenticated endpoint", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[]}`))
	})

	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if meta.Status != string(domain.CredentialActive) {
		t.Errorf("Status = %q, want active", meta.Status)
	}
	if meta.LastVerifiedAt == nil {
		t.Error("LastVerifiedAt was not stamped")
	}
}

// A rejected key is a RESULT, not an error: the provider answered the question.
func TestVerifyReportsARejectedKeyAsAVerdict(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		meta, err := verifyAgainst(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":{"type":"authentication_error","message":"invalid x-api-key"}}`))
		})

		if err != nil {
			t.Fatalf("a %d must be a verdict, not an error: %v", status, err)
		}
		if meta.Status != string(domain.CredentialInvalid) {
			t.Errorf("Status = %q, want invalid", meta.Status)
		}
		if !strings.Contains(meta.LastVerifyError, "invalid x-api-key") {
			t.Errorf("LastVerifyError = %q, want the provider's own message", meta.LastVerifyError)
		}
	}
}

// A 429 or a 5xx says nothing about the key. Treating it as a verdict would
// revoke a working credential over someone else's outage.
func TestVerifyDoesNotCondemnAKeyOverATransientFailure(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway} {
		meta, err := verifyAgainst(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
		})

		if err == nil {
			t.Errorf("a %d should be reported as an error so the caller retries", status)
		}
		if meta.Status == string(domain.CredentialInvalid) {
			t.Errorf("a %d marked the credential invalid", status)
		}
	}
}

// An unreachable endpoint is likewise not the key's fault.
func TestVerifyReportsAnUnreachableEndpoint(t *testing.T) {
	driver := anthropicapi.New(anthropicapi.WithBaseURL("http://127.0.0.1:1"))

	meta, err := driver.Verify(t.Context(), domain.Credential{Secret: testKey})
	if err == nil {
		t.Fatal("an unreachable endpoint must be an error")
	}
	if meta.Status == string(domain.CredentialInvalid) {
		t.Error("an unreachable endpoint marked the credential invalid")
	}
}

func TestVerifyWithNoStoredKey(t *testing.T) {
	meta, err := anthropicapi.New().Verify(t.Context(), domain.Credential{})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if meta.Status != string(domain.CredentialInvalid) {
		t.Errorf("Status = %q, want invalid with no key", meta.Status)
	}
}

// Whatever the provider says in an error body, it must not end up carrying the
// key back to the caller.
func TestVerifyNeverEchoesTheKey(t *testing.T) {
	meta, _ := verifyAgainst(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		// A hostile or careless provider echoing the key back.
		_, _ = w.Write([]byte(`{"error":{"message":"invalid key: ` + testKey + `"}}`))
	})

	if strings.Contains(meta.LastVerifyError, testKey) {
		t.Errorf("the key was echoed into stored metadata: %q", meta.LastVerifyError)
	}
	if strings.Contains(meta.Hint, testKey) {
		t.Error("the hint contains the whole key")
	}
}
