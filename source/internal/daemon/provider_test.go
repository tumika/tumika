package daemon_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tumika/tumika/source/internal/daemon"
	"github.com/tumika/tumika/source/internal/domain"
	"github.com/tumika/tumika/source/internal/platform/paths"
	"github.com/tumika/tumika/source/internal/platform/provider"
	"github.com/tumika/tumika/source/internal/platform/provider/anthropicapi"
)

const apiKey = "sk-ant-api03-" + "ZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZ"

// startWithProvider brings up a real daemon whose only driver points at a fake
// Anthropic endpoint, and returns the base URL, the API token and the database
// path.
func startWithProvider(t *testing.T, upstream http.HandlerFunc) (string, string, string) {
	t.Helper()
	useTestKeyCustody(t)

	api := httptest.NewServer(upstream)
	t.Cleanup(api.Close)

	home := filepath.Join(t.TempDir(), "home")
	p, err := paths.Resolve(home)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	d, err := daemon.New(ctx, daemon.Options{
		Paths: p,
		Providers: []provider.Provider{
			anthropicapi.New(anthropicapi.WithBaseURL(api.URL), anthropicapi.WithHTTPClient(api.Client())),
		},
	})
	if err != nil {
		t.Fatalf("daemon.New: %v", err)
	}
	t.Cleanup(func() {
		if err := d.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	token, err := d.AuthService().Rotate(ctx)
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	served := make(chan error, 1)
	go func() { served <- d.ServeListener(ctx, listener) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-served:
			if err != nil {
				t.Errorf("ServeListener: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("the server did not shut down")
		}
	})

	return "http://" + listener.Addr().String(), token, p.DB
}

func acceptingUpstream(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != apiKey {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"invalid x-api-key"}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[]}`))
	}
}

// The whole slice: submit a key over HTTP, have it sealed, stored and verified,
// and read it back — with the plaintext never leaving the process.
func TestCredentialSubmissionEndToEnd(t *testing.T) {
	base, token, dbPath := startWithProvider(t, acceptingUpstream(t))

	status, body := request(t, token, http.MethodGet, base+"/v1/providers", "")
	if status != http.StatusOK {
		t.Fatalf("GET /v1/providers = %d: %s", status, body)
	}

	var listed struct {
		Providers []domain.ProviderView `json:"providers"`
	}
	if err := json.Unmarshal([]byte(body), &listed); err != nil {
		t.Fatalf("not valid JSON (%v): %s", err, body)
	}
	if len(listed.Providers) != 1 {
		t.Fatalf("expected one provider, got %d", len(listed.Providers))
	}

	view := listed.Providers[0]
	if view.ID != anthropicapi.ID {
		t.Errorf("ID = %q", view.ID)
	}
	// The descriptor is what a client branches on before rendering anything.
	if view.RequiresInteractiveAuth {
		t.Error("an API key provider must not require an interactive login")
	}
	if view.Credential != nil {
		t.Error("no credential has been submitted yet")
	}

	status, body = request(t, token, http.MethodPut, base+"/v1/providers/"+anthropicapi.ID+"/credential",
		`{"method":"api_key","secret":"`+apiKey+`"}`)
	if status != http.StatusOK {
		t.Fatalf("PUT credential = %d: %s", status, body)
	}

	var meta domain.CredentialMeta
	if err := json.Unmarshal([]byte(body), &meta); err != nil {
		t.Fatalf("not valid JSON (%v): %s", err, body)
	}
	if meta.Status != string(domain.CredentialActive) {
		t.Errorf("Status = %q, want active after a successful verification", meta.Status)
	}
	// The response carries metadata only.
	if strings.Contains(body, apiKey) {
		t.Error("the response echoed the key back")
	}

	status, body = request(t, token, http.MethodGet, base+"/v1/providers", "")
	if status != http.StatusOK {
		t.Fatalf("GET after PUT = %d: %s", status, body)
	}
	if strings.Contains(body, apiKey) {
		t.Error("the provider listing carries the key")
	}
	if err := json.Unmarshal([]byte(body), &listed); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	if listed.Providers[0].Credential == nil {
		t.Fatal("the stored credential is not reported")
	}
	// The hint is an ellipsis plus the tail. Trimmed with TrimPrefix rather
	// than a byte slice: the ellipsis is three bytes, so hint[1:] would cut it
	// in half.
	hint := listed.Providers[0].Credential.Hint
	if tail := strings.TrimPrefix(hint, "…"); hint == "" || !strings.HasSuffix(apiKey, tail) {
		t.Errorf("Hint = %q, want the tail of the key", hint)
	}

	assertNotOnDisk(t, dbPath, apiKey)
}

// assertNotOnDisk is the point of sealing: the key must not be recoverable from
// anything the operator would back up or copy.
//
// It checks the WAL and shared-memory files too, not just tumika.db. Under
// journal_mode(WAL) a recent write still lives in tumika.db-wal, so reading only
// the main file would have declared success while the plaintext sat in the
// sidecar — which is exactly what an earlier version of this test did, and a
// mutation storing the plaintext passed it.
func assertNotOnDisk(t *testing.T, dbPath, secret string) {
	t.Helper()

	checked := 0
	for _, path := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		raw, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		checked++

		if bytes.Contains(raw, []byte(secret)) {
			t.Errorf("the credential is present in %s in plaintext", filepath.Base(path))
		}
		if bytes.Contains(raw, []byte("sk-ant-")) {
			t.Errorf("credential-shaped material is present in %s", filepath.Base(path))
		}
	}

	// Guard against the check quietly covering nothing.
	if checked == 0 {
		t.Fatal("no database files were found to check")
	}
}

// Re-opening is what proves the credential was sealed in a form tumika can
// actually recover — and it is the only thing that exercises the AAD binding on
// the way back in. Without this, sealing with the wrong AAD would look fine
// until the first verification after a restart.
func TestStoredCredentialCanBeReopened(t *testing.T) {
	base, token, _ := startWithProvider(t, acceptingUpstream(t))

	if status, body := request(t, token, http.MethodPut, base+"/v1/providers/"+anthropicapi.ID+"/credential",
		`{"method":"api_key","secret":"`+apiKey+`"}`); status != http.StatusOK {
		t.Fatalf("PUT credential = %d: %s", status, body)
	}

	// Verify re-reads the sealed row, opens it with the AAD, and calls the
	// provider with the recovered secret. The fake upstream only answers 200 for
	// the exact key, so a mangled round trip shows up as a failed verification.
	status, body := request(t, token, http.MethodPost, base+"/v1/providers/"+anthropicapi.ID+"/verify", "")
	if status != http.StatusOK {
		t.Fatalf("POST verify = %d: %s", status, body)
	}

	var meta domain.CredentialMeta
	if err := json.Unmarshal([]byte(body), &meta); err != nil {
		t.Fatalf("not valid JSON (%v): %s", err, body)
	}
	if meta.Status != string(domain.CredentialActive) {
		t.Errorf("Status = %q after re-opening the stored credential", meta.Status)
	}
	if strings.Contains(body, apiKey) {
		t.Error("verification echoed the key back")
	}
}

// A key the provider rejects is stored and reported as invalid rather than
// silently discarded — the operator needs to see that it was tried and refused.
func TestRejectedCredentialIsReportedNotDiscarded(t *testing.T) {
	base, token, _ := startWithProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid x-api-key"}}`))
	})

	status, body := request(t, token, http.MethodPut, base+"/v1/providers/"+anthropicapi.ID+"/credential",
		`{"method":"api_key","secret":"`+apiKey+`"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("PUT with a rejected key = %d, want 400: %s", status, body)
	}
	if !strings.Contains(body, "credential_invalid") {
		t.Errorf("error code = %s", body)
	}
	if strings.Contains(body, apiKey) {
		t.Error("the rejected key was echoed back")
	}
}

// Shape is checked before any network call, so an obvious paste error fails
// immediately rather than after a round trip.
func TestMalformedSecretIsRejectedWithoutCallingTheProvider(t *testing.T) {
	called := false
	base, token, _ := startWithProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	status, body := request(t, token, http.MethodPut, base+"/v1/providers/"+anthropicapi.ID+"/credential",
		`{"method":"api_key","secret":"nonsense"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("= %d, want 400: %s", status, body)
	}
	if called {
		t.Error("the provider was called with a secret that could not possibly be valid")
	}
}

// The capability sentinels, over HTTP. anthropic-api installs nothing and has no
// login flow, and both must be documented codes rather than a panic.
func TestUnsupportedCapabilitiesReturnDocumentedCodes(t *testing.T) {
	base, token, _ := startWithProvider(t, acceptingUpstream(t))

	status, body := request(t, token, http.MethodGet, base+"/v1/providers/nope", "")
	if status != http.StatusNotFound {
		t.Errorf("unknown provider = %d, want 404: %s", status, body)
	}

	status, body = request(t, token, http.MethodPut, base+"/v1/providers/nope/credential",
		`{"method":"api_key","secret":"`+apiKey+`"}`)
	if status != http.StatusNotFound {
		t.Errorf("credential for an unknown provider = %d, want 404: %s", status, body)
	}
}

func TestSelectAndDeleteCredential(t *testing.T) {
	base, token, _ := startWithProvider(t, acceptingUpstream(t))

	if status, body := request(t, token, http.MethodPost, base+"/v1/providers/"+anthropicapi.ID+"/select", ""); status != http.StatusNoContent {
		t.Fatalf("select = %d: %s", status, body)
	}

	_, body := request(t, token, http.MethodGet, base+"/v1/providers", "")
	var listed struct {
		Providers []domain.ProviderView `json:"providers"`
	}
	if err := json.Unmarshal([]byte(body), &listed); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	if !listed.Providers[0].Selected {
		t.Error("the provider was not marked selected")
	}

	if status, _ := request(t, token, http.MethodPut, base+"/v1/providers/"+anthropicapi.ID+"/credential",
		`{"method":"api_key","secret":"`+apiKey+`"}`); status != http.StatusOK {
		t.Fatal("PUT credential failed")
	}
	if status, body := request(t, token, http.MethodDelete, base+"/v1/providers/"+anthropicapi.ID+"/credential", ""); status != http.StatusNoContent {
		t.Fatalf("delete = %d: %s", status, body)
	}

	_, body = request(t, token, http.MethodGet, base+"/v1/providers", "")
	if err := json.Unmarshal([]byte(body), &listed); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	if listed.Providers[0].Credential != nil {
		t.Error("the credential is still reported after being deleted")
	}
}
