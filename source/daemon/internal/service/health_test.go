package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tumika/tumika/source/daemon/internal/domain"
	"github.com/tumika/tumika/source/daemon/internal/platform/buildinfo"
	"github.com/tumika/tumika/source/daemon/internal/service"
)

// healthDeps is the self-report's dependencies with no settings and no updater,
// which is what a Snapshot test needs: both belong to the version report.
func healthDeps(
	version string,
	schema service.SchemaVersionFunc,
	auth service.AuthService,
	backend string,
) service.HealthDeps {
	return service.HealthDeps{
		Build:          buildinfo.Info{Version: version},
		Started:        time.Now(),
		Schema:         schema,
		Auth:           auth,
		SecretsBackend: backend,
	}
}

func TestHealthReportsAHealthyDaemon(t *testing.T) {
	auth, _ := newAuth(t)
	if _, err := auth.Rotate(t.Context()); err != nil {
		t.Fatalf("Rotate: %v", err)
	}

	schema := func(context.Context) (int64, error) { return 1, nil }
	deps := healthDeps("v1.2.3", schema, auth, "file")
	deps.Started = time.Now().Add(-90 * time.Second)
	svc := service.NewHealthService(deps)

	h := svc.Snapshot(t.Context())

	if h.Status != "ok" {
		t.Errorf("Status = %q, want ok: %+v", h.Status, h.Warnings)
	}
	if h.Version != "v1.2.3" {
		t.Errorf("Version = %q", h.Version)
	}
	if h.Uptime != "1m30s" {
		t.Errorf("Uptime = %q, want 1m30s", h.Uptime)
	}
	if !h.Database.Reachable || h.Database.SchemaVersion != 1 {
		t.Errorf("Database = %+v", h.Database)
	}
	if !h.Auth.TokenConfigured {
		t.Error("TokenConfigured = false after a token was minted")
	}
	if len(h.Warnings) != 0 {
		t.Errorf("Warnings = %v, want none", h.Warnings)
	}
}

// A health check that fails to report is the least useful failure there is: the
// caller learns only that something, somewhere, is wrong. Each component's
// failure belongs in the report.
func TestHealthDegradesRatherThanFailing(t *testing.T) {
	auth, _ := newAuth(t) // deliberately no token minted

	schema := func(context.Context) (int64, error) { return 0, errors.New("database is gone") }
	svc := service.NewHealthService(healthDeps("dev", schema, auth, "file"))

	h := svc.Snapshot(t.Context())

	if h.Status != "degraded" {
		t.Errorf("Status = %q, want degraded", h.Status)
	}
	if h.Database.Reachable {
		t.Error("Reachable = true with a failing schema lookup")
	}
	if !strings.Contains(h.Database.Error, "database is gone") {
		t.Errorf("Database.Error = %q, want the underlying failure", h.Database.Error)
	}
	if len(h.Warnings) != 2 {
		t.Errorf("Warnings = %v, want one for the database and one for the missing token", h.Warnings)
	}
	if h.Auth.TokenConfigured {
		t.Error("TokenConfigured = true with no token")
	}
}

// The report must never carry the token or its hash — only whether one exists.
func TestHealthNeverCarriesTheToken(t *testing.T) {
	auth, _ := newAuth(t)
	minted, err := auth.Rotate(t.Context())
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	token := minted.Token

	schema := func(context.Context) (int64, error) { return 1, nil }
	h := service.NewHealthService(healthDeps("dev", schema, auth, "file")).Snapshot(t.Context())

	rendered := strings.Join(append(h.Warnings,
		h.Status, h.Version, h.Uptime, h.Database.Error), " ")
	if strings.Contains(rendered, token) {
		t.Error("the API token appears in the health report")
	}
}

// The backend is reported so an operator can tell which custody is in use —
// which matters because restoring a database to a new host loses credentials
// sealed by a host-bound backend (ADR-0002).
func TestHealthReportsTheSecretsBackend(t *testing.T) {
	auth, _ := newAuth(t)
	if _, err := auth.Rotate(t.Context()); err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	schema := func(context.Context) (int64, error) { return 1, nil }

	h := service.NewHealthService(healthDeps("dev", schema, auth, "keychain")).Snapshot(t.Context())
	if h.Secrets.Backend != "keychain" {
		t.Errorf("Secrets.Backend = %q, want keychain", h.Secrets.Backend)
	}
	if h.Status != "ok" {
		t.Errorf("Status = %q, warnings %v", h.Status, h.Warnings)
	}

	// No custody at all means credentials cannot be stored; that is degraded,
	// not fine.
	degraded := service.NewHealthService(healthDeps("dev", schema, auth, "")).Snapshot(t.Context())
	if degraded.Status != "degraded" {
		t.Errorf("Status = %q with no key custody, want degraded", degraded.Status)
	}
}

// stubUpdateState is the one method the version report reads from the updater.
type stubUpdateState struct {
	state domain.UpdateState
	err   error
}

func (s stubUpdateState) State(context.Context) (domain.UpdateState, error) {
	return s.state, s.err
}

// failingSettings is a settings table that cannot be read, which is the state a
// daemon is in when someone asks it what it is running and why it is broken.
type failingSettings struct{}

func (failingSettings) Get(context.Context, string) (domain.SettingView, error) {
	return domain.SettingView{}, errors.New("database is locked")
}

// versionDeps is the version report's dependencies over a real config service.
func versionDeps(t *testing.T) (service.HealthDeps, service.ConfigService) {
	t.Helper()
	auth, _ := newAuth(t)
	cfg, _, _ := newService(t)

	deps := healthDeps("0.0.2", func(context.Context) (int64, error) { return 1, nil }, auth, "file")
	deps.Build = buildinfo.Info{Version: "0.0.2", Release: "2026.09.00", SchemaVersion: 7}
	deps.Settings = cfg
	return deps, cfg
}

// The release and the embedded schema version are reported alongside the
// component version: the release is what a bill of materials is looked up by,
// and the schema is what a binary about to be swapped in is compared on.
func TestVersionReportsTheReleaseTheChannelAndTheSchema(t *testing.T) {
	deps, cfg := versionDeps(t)
	if _, err := cfg.Set(t.Context(), map[string]json.RawMessage{
		service.KeyUpdateChannel: json.RawMessage(`"beta"`),
	}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	report := service.NewHealthService(deps).Version(t.Context())

	if report.Version != "0.0.2" || report.Release != "2026.09.00" {
		t.Errorf("version = %q, release = %q", report.Version, report.Release)
	}
	if report.SchemaVersion != 7 {
		t.Errorf("SchemaVersion = %d, want 7", report.SchemaVersion)
	}
	if report.Channel != "beta" {
		t.Errorf("Channel = %q, want the configured channel", report.Channel)
	}
}

// An unset channel reports the setting's default rather than nothing: a client
// deciding what this daemon follows must not have to know the default itself.
func TestVersionReportsTheDefaultChannelWhenUnset(t *testing.T) {
	deps, _ := versionDeps(t)

	if got := service.NewHealthService(deps).Version(t.Context()).Channel; got != "stable" {
		t.Errorf("Channel = %q, want stable", got)
	}
}

// The report answers on a broken install — that is when it is consulted — so an
// unreadable setting costs the channel and nothing else.
func TestVersionSurvivesUnreadableSettings(t *testing.T) {
	deps, _ := versionDeps(t)
	deps.Settings = failingSettings{}

	report := service.NewHealthService(deps).Version(t.Context())

	if report.Channel != "" {
		t.Errorf("Channel = %q, want it left out", report.Channel)
	}
	if report.Version != "0.0.2" {
		t.Errorf("Version = %q, want the build to be reported anyway", report.Version)
	}
}

// "Which release am I running" and "is an update half-applied" are the same
// question after a daemon has restarted itself onto a new binary.
func TestVersionCarriesUpdateState(t *testing.T) {
	deps, _ := versionDeps(t)
	deps.Updates = stubUpdateState{state: domain.UpdateState{
		Status: domain.UpdatePending, ToVersion: "0.0.3",
	}}

	report := service.NewHealthService(deps).Version(t.Context())

	if report.Update == nil || report.Update.ToVersion != "0.0.3" {
		t.Errorf("Update = %+v, want the pending update", report.Update)
	}
}

func TestVersionOmitsUpdateStateItCannotReport(t *testing.T) {
	deps, _ := versionDeps(t)

	// No updater at all: self-update is disabled for this install.
	if report := service.NewHealthService(deps).Version(t.Context()); report.Update != nil {
		t.Errorf("Update = %+v with no updater", report.Update)
	}

	// An updater whose row cannot be read. The updater execs `version` to
	// pre-flight a staged binary, so the rest of the report still has to answer.
	deps.Updates = stubUpdateState{err: errors.New("database is locked")}
	report := service.NewHealthService(deps).Version(t.Context())
	if report.Update != nil {
		t.Errorf("Update = %+v, want an unreadable state left out", report.Update)
	}
	if report.Version != "0.0.2" {
		t.Errorf("Version = %q, want the build to be reported anyway", report.Version)
	}
}
