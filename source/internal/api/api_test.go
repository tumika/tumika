package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tumika/tumika/source/internal/api"
	"github.com/tumika/tumika/source/internal/domain"
	"github.com/tumika/tumika/source/internal/service"
)

// fakeConfigService lets the transport layer be tested on its own: no database,
// no migration, no business logic. If a handler test ever needs a real service
// to pass, logic has leaked into the handler.
type fakeConfigService struct {
	views    []domain.SettingView
	setErr   error
	resetErr error
	getErr   error

	lastSet   map[string]json.RawMessage
	lastReset string
	setCalls  int
}

func (f *fakeConfigService) Definitions() []domain.SettingDefinition { return nil }

func (f *fakeConfigService) Get(context.Context, string) (domain.SettingView, error) {
	if f.getErr != nil {
		return domain.SettingView{}, f.getErr
	}
	return domain.SettingView{}, nil
}

func (f *fakeConfigService) List(context.Context) ([]domain.SettingView, error) {
	return f.views, nil
}

func (f *fakeConfigService) Set(_ context.Context, v map[string]json.RawMessage) ([]domain.SettingView, error) {
	f.setCalls++
	f.lastSet = v
	if f.setErr != nil {
		return nil, f.setErr
	}
	return f.views, nil
}

func (f *fakeConfigService) Reset(_ context.Context, key string) error {
	f.lastReset = key
	return f.resetErr
}

func do(t *testing.T, svc *fakeConfigService, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()

	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}

	req := httptest.NewRequest(method, target, reader)
	rec := httptest.NewRecorder()
	api.NewRouter(api.Deps{Config: svc}).ServeHTTP(rec, req)
	return rec
}

func TestListConfig(t *testing.T) {
	svc := &fakeConfigService{views: []domain.SettingView{{
		Key:     "server.listen",
		Kind:    domain.SettingAddress,
		Value:   json.RawMessage(`"127.0.0.1:8737"`),
		Default: json.RawMessage(`"127.0.0.1:8737"`),
	}}}

	rec := do(t, svc, http.MethodGet, "/v1/config", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q", ct)
	}
	// Configuration must not sit in an intermediary's cache.
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}

	var body struct {
		Settings []domain.SettingView `json:"settings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not valid JSON (%v): %s", err, rec.Body)
	}
	if len(body.Settings) != 1 || body.Settings[0].Key != "server.listen" {
		t.Errorf("unexpected body: %s", rec.Body)
	}
}

func TestPatchConfigPassesValuesThroughUntouched(t *testing.T) {
	svc := &fakeConfigService{}

	rec := do(t, svc, http.MethodPatch, "/v1/config",
		`{"settings":{"update.auto_apply":true,"update.check_interval":"30m"}}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	if svc.setCalls != 1 {
		t.Fatalf("service called %d times, want exactly 1", svc.setCalls)
	}
	// The handler must not interpret values — deciding what "30m" means for a
	// given key is the service's job.
	if got := string(svc.lastSet["update.check_interval"]); got != `"30m"` {
		t.Errorf("value reached the service as %s, want it verbatim", got)
	}
	if got := string(svc.lastSet["update.auto_apply"]); got != "true" {
		t.Errorf("value reached the service as %s, want it verbatim", got)
	}
}

// Sentinel errors are translated into codes and statuses. That is encoding, not
// deciding: the service already decided by returning a particular error.
func TestServiceErrorsMapToStatusAndCode(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantCode int
		wantBody string
	}{
		{"unknown setting", fmt.Errorf("%w: %q", service.ErrUnknownSetting, "nope"), http.StatusNotFound, "unknown_setting"},
		{"invalid value", fmt.Errorf("%w: bad", service.ErrInvalidSetting), http.StatusBadRequest, "invalid_setting"},
		{"anything else", errors.New("database on fire"), http.StatusInternalServerError, "internal"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc := &fakeConfigService{setErr: tc.err}

			rec := do(t, svc, http.MethodPatch, "/v1/config", `{"settings":{"update.auto_apply":true}}`)

			if rec.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.wantCode, rec.Body)
			}

			var body struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("error response is not the envelope: %s", rec.Body)
			}
			if body.Error.Code != tc.wantBody {
				t.Errorf("code = %q, want %q", body.Error.Code, tc.wantBody)
			}
			if body.Error.Message == "" {
				t.Error("the envelope must carry a message")
			}
		})
	}
}

// An internal failure's text can carry paths, SQL or driver detail. None of it
// belongs in a response.
func TestInternalErrorsDoNotLeakDetail(t *testing.T) {
	svc := &fakeConfigService{setErr: errors.New("/var/lib/tumika/tumika.db: disk I/O error near line 42")}

	rec := do(t, svc, http.MethodPatch, "/v1/config", `{"settings":{"update.auto_apply":true}}`)

	if strings.Contains(rec.Body.String(), "/var/lib/tumika") || strings.Contains(rec.Body.String(), "disk I/O") {
		t.Errorf("internal detail leaked into the response: %s", rec.Body)
	}
}

func TestMalformedRequestsAreRejected(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"not JSON", `{`},
		{"unknown field", `{"setting":{"a":1}}`},
		{"trailing content", `{"settings":{}} {"settings":{}}`},
		{"empty body", ``},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc := &fakeConfigService{}

			rec := do(t, svc, http.MethodPatch, "/v1/config", tc.body)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body)
			}
			if svc.setCalls != 0 {
				t.Error("a malformed request must not reach the service")
			}
		})
	}
}

// A misspelled field silently ignored would report success for a change that
// never happened, which is worse than an error.
func TestUnknownFieldsAreNotSilentlyIgnored(t *testing.T) {
	svc := &fakeConfigService{}

	rec := do(t, svc, http.MethodPatch, "/v1/config", `{"settings":{"update.auto_apply":true},"extra":1}`)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for an unknown field", rec.Code)
	}
}

func TestOversizedBodyIsRejected(t *testing.T) {
	svc := &fakeConfigService{}
	huge := `{"settings":{"provider.selected":"` + strings.Repeat("a", 300<<10) + `"}}`

	rec := do(t, svc, http.MethodPatch, "/v1/config", huge)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for an oversized body", rec.Code)
	}
	if svc.setCalls != 0 {
		t.Error("an oversized body must not reach the service")
	}
}

func TestResetConfig(t *testing.T) {
	svc := &fakeConfigService{}

	rec := do(t, svc, http.MethodDelete, "/v1/config/server.listen", "")

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204: %s", rec.Code, rec.Body)
	}
	if svc.lastReset != "server.listen" {
		t.Errorf("reset key = %q, want the path value", svc.lastReset)
	}
}

func TestUnroutedRequests(t *testing.T) {
	tests := []struct {
		method, target string
		want           int
	}{
		{http.MethodPost, "/v1/config", http.StatusMethodNotAllowed},
		{http.MethodGet, "/v1/nope", http.StatusNotFound},
		{http.MethodGet, "/", http.StatusNotFound},
	}

	for _, tc := range tests {
		t.Run(tc.method+" "+tc.target, func(t *testing.T) {
			rec := do(t, &fakeConfigService{}, tc.method, tc.target, "")
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}
