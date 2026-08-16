package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tumika/tumika/source/internal/api"
	"github.com/tumika/tumika/source/internal/domain"
)

// fakeProviderService exercises the handlers on their own: no database, no
// registry, no driver. A handler test that needed those would mean logic had
// leaked out of the service.
type fakeProviderService struct {
	views     []domain.ProviderView
	meta      domain.CredentialMeta
	preflight domain.Preflight
	err       error

	install domain.InstallResult

	lastID      string
	lastMethod  domain.AuthMethod
	lastSecret  string
	lastVersion string
	calls       int
}

func (f *fakeProviderService) Seed(context.Context) error { return f.err }

func (f *fakeProviderService) List(context.Context) ([]domain.ProviderView, error) {
	return f.views, f.err
}

func (f *fakeProviderService) Get(_ context.Context, id string) (domain.ProviderView, error) {
	f.lastID = id
	if f.err != nil {
		return domain.ProviderView{}, f.err
	}
	return f.views[0], nil
}

func (f *fakeProviderService) Preflight(_ context.Context, id string) (domain.Preflight, error) {
	f.lastID = id
	return f.preflight, f.err
}

func (f *fakeProviderService) Select(_ context.Context, id string) error {
	f.lastID = id
	return f.err
}

func (f *fakeProviderService) Install(_ context.Context, id, version string) (domain.InstallResult, error) {
	f.calls++
	f.lastID, f.lastVersion = id, version
	return f.install, f.err
}

func (f *fakeProviderService) SubmitSecret(
	_ context.Context, id string, method domain.AuthMethod, secret string,
) (domain.CredentialMeta, error) {
	f.calls++
	f.lastID, f.lastMethod, f.lastSecret = id, method, secret
	return f.meta, f.err
}

func (f *fakeProviderService) StoreCredential(context.Context, domain.Credential) (domain.CredentialMeta, error) {
	return f.meta, f.err
}

func (f *fakeProviderService) VerifyCredential(_ context.Context, id string) (domain.CredentialMeta, error) {
	f.lastID = id
	return f.meta, f.err
}

func (f *fakeProviderService) DeleteCredential(_ context.Context, id string) error {
	f.lastID = id
	return f.err
}

func doProvider(t *testing.T, svc *fakeProviderService, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(method, "http://127.0.0.1:8737"+target, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tmk_test")

	rec := httptest.NewRecorder()
	api.NewRouter(api.Deps{
		Config:    &fakeConfigService{},
		Providers: svc,
		Health:    stubHealth{},
		Auth:      allowAll{},
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}).ServeHTTP(rec, req)
	return rec
}

// doProviderChunked sends a request with no body and an unknown length, which
// is what net/http reports as ContentLength -1.
func doProviderChunked(t *testing.T, svc *fakeProviderService, method, target string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(method, "http://127.0.0.1:8737"+target, strings.NewReader(""))
	req.Header.Set("Authorization", "Bearer tmk_test")
	req.ContentLength = -1
	req.TransferEncoding = []string{"chunked"}

	rec := httptest.NewRecorder()
	api.NewRouter(api.Deps{
		Config:    &fakeConfigService{},
		Providers: svc,
		Health:    stubHealth{},
		Auth:      allowAll{},
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}).ServeHTTP(rec, req)
	return rec
}

func TestListProviders(t *testing.T) {
	svc := &fakeProviderService{views: []domain.ProviderView{{
		Descriptor:              domain.Descriptor{ID: "anthropic-api", DisplayName: "Anthropic API"},
		RequiresInteractiveAuth: false,
	}}}

	rec := doProvider(t, svc, http.MethodGet, "/v1/providers", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var body struct {
		Providers []domain.ProviderView `json:"providers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("not valid JSON (%v): %s", err, rec.Body)
	}
	if len(body.Providers) != 1 || body.Providers[0].ID != "anthropic-api" {
		t.Errorf("body = %s", rec.Body)
	}
}

// The handler passes the secret through untouched: deciding what a method means
// for a given provider is the service's job.
func TestPutCredentialPassesTheSecretThrough(t *testing.T) {
	svc := &fakeProviderService{meta: domain.CredentialMeta{Status: "active", Hint: "…abcd"}}

	rec := doProvider(t, svc, http.MethodPut, "/v1/providers/anthropic-api/credential",
		`{"method":"api_key","secret":"sk-ant-secret-value"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if svc.calls != 1 {
		t.Fatalf("service called %d times, want 1", svc.calls)
	}
	if svc.lastID != "anthropic-api" || svc.lastMethod != domain.AuthAPIKey {
		t.Errorf("id=%q method=%q", svc.lastID, svc.lastMethod)
	}
	if svc.lastSecret != "sk-ant-secret-value" {
		t.Errorf("the secret was altered: %q", svc.lastSecret)
	}
	// Only metadata comes back.
	if strings.Contains(rec.Body.String(), "sk-ant-secret-value") {
		t.Errorf("the response echoed the secret: %s", rec.Body)
	}
}

// Sentinel errors map to documented codes. This is encoding, not deciding — the
// service already decided by returning a particular error.
func TestProviderErrorsMapToStatusAndCode(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantCode int
		wantBody string
	}{
		{"unknown provider", fmt.Errorf("%w: nope", domain.ErrNotFound), http.StatusNotFound, "not_found"},
		{"rejected credential", fmt.Errorf("%w: bad", domain.ErrCredentialInvalid), http.StatusBadRequest, "credential_invalid"},
		{"needs a login session", fmt.Errorf("%w: x", domain.ErrInteractiveAuthRequired), http.StatusBadRequest, "interactive_auth_required"},
		{"no login flow", fmt.Errorf("%w: x", domain.ErrInteractiveAuthUnsupported), http.StatusBadRequest, "interactive_auth_unsupported"},
		{"installs nothing", fmt.Errorf("%w: x", domain.ErrInstallUnsupported), http.StatusBadRequest, "install_unsupported"},
		// The provider failing is not tumika failing; a 500 would send the
		// operator to look at the wrong system.
		{"provider unreachable", fmt.Errorf("%w: x", domain.ErrProviderUnavailable), http.StatusBadGateway, "provider_unavailable"},
		{"verdict arrived too late", fmt.Errorf("%w: x", domain.ErrSuperseded), http.StatusConflict, "superseded"},
		{"anything else", errors.New("database on fire"), http.StatusInternalServerError, "internal"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc := &fakeProviderService{err: tc.err}

			rec := doProvider(t, svc, http.MethodPut, "/v1/providers/anthropic-api/credential",
				`{"method":"api_key","secret":"sk-ant-x"}`)

			if rec.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.wantCode, rec.Body)
			}
			if !strings.Contains(rec.Body.String(), tc.wantBody) {
				t.Errorf("body = %s, want code %q", rec.Body, tc.wantBody)
			}
			if tc.wantCode == http.StatusInternalServerError && strings.Contains(rec.Body.String(), "database on fire") {
				t.Error("internal detail leaked into the response")
			}
		})
	}
}

// An explicit verification answers with the metadata even when the credential is
// rejected — the caller asked a question and this is the answer.
func TestVerifyAnswersWithMetadata(t *testing.T) {
	svc := &fakeProviderService{meta: domain.CredentialMeta{
		Status:          string(domain.CredentialInvalid),
		LastVerifyError: "rejected by the provider",
		Hint:            "…abcd",
	}}

	rec := doProvider(t, svc, http.MethodPost, "/v1/providers/anthropic-api/verify", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for a rejection reported as a result: %s", rec.Code, rec.Body)
	}
	var meta domain.CredentialMeta
	if err := json.Unmarshal(rec.Body.Bytes(), &meta); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	if meta.Status != string(domain.CredentialInvalid) || meta.LastVerifyError == "" {
		t.Errorf("the caller cannot tell why: %+v", meta)
	}
}

func TestProviderRoutes(t *testing.T) {
	tests := []struct {
		method, target string
		body           string
		want           int
	}{
		{http.MethodGet, "/v1/providers/anthropic-api", "", http.StatusOK},
		{http.MethodGet, "/v1/providers/anthropic-api/preflight", "", http.StatusOK},
		{http.MethodPost, "/v1/providers/anthropic-api/select", "", http.StatusNoContent},
		{http.MethodDelete, "/v1/providers/anthropic-api/credential", "", http.StatusNoContent},
	}

	for _, tc := range tests {
		t.Run(tc.method+" "+tc.target, func(t *testing.T) {
			svc := &fakeProviderService{
				views:     []domain.ProviderView{{Descriptor: domain.Descriptor{ID: "anthropic-api"}}},
				preflight: domain.Preflight{Ready: true},
			}

			rec := doProvider(t, svc, tc.method, tc.target, tc.body)

			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.want, rec.Body)
			}
			if svc.lastID != "anthropic-api" {
				t.Errorf("the path value did not reach the service: %q", svc.lastID)
			}
		})
	}
}

func TestPutCredentialRejectsAMalformedBody(t *testing.T) {
	for _, body := range []string{`{`, `{"secret":"x","unknown":1}`, ``} {
		svc := &fakeProviderService{}

		rec := doProvider(t, svc, http.MethodPut, "/v1/providers/anthropic-api/credential", body)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %q = %d, want 400", body, rec.Code)
		}
		if svc.calls != 0 {
			t.Error("a malformed request reached the service")
		}
	}
}

// An install with no body means the pinned version, so a client can ask for
// "install it" without knowing which number that is.
func TestInstallWithoutABodyMeansThePinnedVersion(t *testing.T) {
	svc := &fakeProviderService{install: domain.InstallResult{
		Version: "2.1.233",
		Path:    "/var/lib/tumika/providers/claude-code/2.1.233/claude",
	}}

	rec := doProvider(t, svc, http.MethodPost, "/v1/providers/claude-code/install", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if svc.calls != 1 {
		t.Fatalf("service called %d times, want 1", svc.calls)
	}
	if svc.lastID != "claude-code" {
		t.Errorf("id = %q", svc.lastID)
	}
	if svc.lastVersion != "" {
		t.Errorf("version = %q, want empty so the driver chooses its pin", svc.lastVersion)
	}
	if !strings.Contains(rec.Body.String(), "2.1.233") {
		t.Errorf("the response does not say what was installed: %s", rec.Body)
	}
}

// An explicit version is passed through, so an operator can stage a bump ahead
// of the daemon that will run it.
func TestInstallPassesAnExplicitVersionThrough(t *testing.T) {
	svc := &fakeProviderService{install: domain.InstallResult{Version: "2.1.240"}}

	rec := doProvider(t, svc, http.MethodPost, "/v1/providers/claude-code/install",
		`{"version":"2.1.240"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if svc.lastVersion != "2.1.240" {
		t.Errorf("version = %q, want it passed through", svc.lastVersion)
	}
}

// A provider that vendors nothing is a client mistake, not a server failure —
// and the registry answers that by type assertion, so the handler only encodes.
func TestInstallOnAProviderThatVendorsNothing(t *testing.T) {
	svc := &fakeProviderService{err: domain.ErrInstallUnsupported}

	rec := doProvider(t, svc, http.MethodPost, "/v1/providers/anthropic-api/install", "")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "install_unsupported") {
		t.Errorf("body = %s, want the documented code", rec.Body)
	}
}

func TestInstallRejectsAMalformedBody(t *testing.T) {
	for _, body := range []string{`{`, `{"version":"2.1.240","unknown":1}`} {
		svc := &fakeProviderService{}

		rec := doProvider(t, svc, http.MethodPost, "/v1/providers/claude-code/install", body)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %q = %d, want 400", body, rec.Code)
		}
		if svc.calls != 0 {
			t.Errorf("body %q reached the service", body)
		}
	}
}

// A client that sends no body with Transfer-Encoding: chunked gets
// ContentLength -1, not 0. Keying on that answered "install the pinned version"
// with a 400.
func TestInstallAcceptsAnEmptyChunkedBody(t *testing.T) {
	svc := &fakeProviderService{install: domain.InstallResult{Version: "2.1.233"}}

	rec := doProviderChunked(t, svc, http.MethodPost, "/v1/providers/claude-code/install")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	if svc.lastVersion != "" {
		t.Errorf("version = %q, want empty so the driver chooses its pin", svc.lastVersion)
	}
}
