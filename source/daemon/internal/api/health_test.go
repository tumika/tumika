package api_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tumika/tumika/source/daemon/internal/api"
	"github.com/tumika/tumika/source/daemon/internal/domain"
	"github.com/tumika/tumika/source/daemon/internal/platform/buildinfo"
	"github.com/tumika/tumika/source/daemon/internal/service"
)

// doVersion drives GET /v1/version with a given report behind the service.
func doVersion(t *testing.T, report service.VersionReport) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8737/v1/version", nil)
	req.Header.Set("Authorization", "Bearer tmk_test")

	rec := httptest.NewRecorder()
	api.NewRouter(api.Deps{
		Config: &fakeConfigService{},
		Health: stubHealth{report: report},
		Auth:   allowAll{},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}).ServeHTTP(rec, req)
	return rec
}

// The release, the channel and the embedded schema version travel together with
// the component version: the desktop app pairs on the release, and the updater
// compares schemas before it swaps a binary.
func TestVersionReportsReleaseChannelAndSchema(t *testing.T) {
	rec := doVersion(t, service.VersionReport{
		Info: buildinfo.Info{
			Version:       "0.0.2",
			Release:       "2026.09.00",
			SchemaVersion: 7,
		},
		Channel: "beta",
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}

	var body struct {
		Version       string `json:"version"`
		Release       string `json:"release"`
		Channel       string `json:"channel"`
		SchemaVersion int64  `json:"schema_version"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not valid JSON (%v): %s", err, rec.Body)
	}

	if body.Version != "0.0.2" || body.Release != "2026.09.00" {
		t.Errorf("version = %q, release = %q", body.Version, body.Release)
	}
	if body.Channel != "beta" {
		t.Errorf("channel = %q, want beta", body.Channel)
	}
	if body.SchemaVersion != 7 {
		t.Errorf("schema_version = %d, want 7", body.SchemaVersion)
	}
}

// Update state joins /v1/version, because "which release am I running" and "is
// an update half-applied" are the same question after a daemon restarts itself.
func TestVersionCarriesUpdateState(t *testing.T) {
	rec := doVersion(t, service.VersionReport{
		Update: &domain.UpdateState{Status: domain.UpdatePending, ToVersion: "0.2.0"},
	})

	var body struct {
		Update *domain.UpdateState `json:"update"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not valid JSON (%v): %s", err, rec.Body)
	}
	if body.Update == nil || body.Update.ToVersion != "0.2.0" {
		t.Errorf("version does not carry update state: %s", rec.Body)
	}
}
