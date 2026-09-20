package service

import (
	"context"
	"time"

	"github.com/tumika/tumika/source/daemon/internal/domain"
	"github.com/tumika/tumika/source/daemon/internal/platform/buildinfo"
)

// SchemaVersionFunc reports the database's migration version.
//
// A function rather than an interface because it is one call, and taking the
// store itself would put a concrete repository implementation in a service's
// dependencies — which depguard forbids, and rightly: it is what would stop
// HealthService being testable without a database.
type SchemaVersionFunc func(ctx context.Context) (int64, error)

// UpdateStateReader is the one method the version report needs from the
// updater: a report that also answers "is an update half-applied" has no
// business holding the interface that can apply one.
type UpdateStateReader interface {
	State(ctx context.Context) (domain.UpdateState, error)
}

// VersionReport is the build identity of the running binary, the channel it
// follows and whatever the self-updater is doing.
//
// One report rather than two: "which release am I running" and "is an update
// half-applied" are the same question when a daemon has just restarted itself,
// and answering them separately invites reading one without the other.
type VersionReport struct {
	buildinfo.Info
	// Channel is the update channel this daemon follows. Empty when the
	// setting cannot be read — the report has to answer on an install whose
	// database is broken, which is exactly when it is being consulted.
	Channel string `json:"channel"`
	// Update is omitted when self-update does not apply to this install, and
	// when the update row cannot be read.
	Update *domain.UpdateState `json:"update,omitempty"`
}

// HealthService assembles the daemon's self-report.
type HealthService interface {
	Snapshot(ctx context.Context) domain.Health
	// Version never returns an error, for the same reason Snapshot does not:
	// it is what the updater consults about a binary it is unsure of.
	Version(ctx context.Context) VersionReport
}

// HealthDeps is what the self-report is assembled from.
type HealthDeps struct {
	// Build is the build identity of this binary, read once at startup.
	Build buildinfo.Info
	// Started is the process start time, so uptime survives however long the
	// first request takes to arrive.
	Started time.Time
	Schema  SchemaVersionFunc
	Auth    AuthService
	// SecretsBackend names the credential key custody in use. Empty means none
	// is configured, which is degraded rather than fine.
	SecretsBackend string
	// Settings is narrowed to reading: the channel is owned by ConfigService,
	// and a report has no reason to write a setting or reach a secret one.
	Settings SettingGetter
	// Updates is nil where self-update is disabled — a development build, or a
	// container with no supervisor to relaunch it.
	Updates UpdateStateReader
}

type healthService struct {
	deps HealthDeps
	now  func() time.Time
}

// NewHealthService builds the service.
func NewHealthService(deps HealthDeps) HealthService {
	return &healthService{deps: deps, now: time.Now}
}

// Version assembles the build identity, the channel and the update state.
//
// Every part that cannot be read is left out rather than turned into an error:
// this is the report an operator reads when an install is broken, and one
// unreadable row must not take the rest of it down.
func (s *healthService) Version(ctx context.Context) VersionReport {
	report := VersionReport{Info: s.deps.Build}

	if s.deps.Settings != nil {
		if channel, err := String(ctx, s.deps.Settings, KeyUpdateChannel); err == nil {
			report.Channel = channel
		}
	}

	if s.deps.Updates != nil {
		if state, err := s.deps.Updates.State(ctx); err == nil {
			report.Update = &state
		}
	}

	return report
}

// Snapshot never returns an error.
//
// A health check that fails to report is the least useful failure mode there is:
// the caller learns nothing except that something is wrong somewhere. Each
// component's failure is captured in the report instead, and degrades Status.
func (s *healthService) Snapshot(ctx context.Context) domain.Health {
	h := domain.Health{
		Status:  "ok",
		Version: s.deps.Build.Version,
		Started: s.deps.Started,
		Uptime:  s.now().Sub(s.deps.Started).Round(time.Second).String(),
	}

	if version, err := s.deps.Schema(ctx); err != nil {
		h.Database.Error = err.Error()
		h.Warnings = append(h.Warnings, "database is unreachable")
		h.Status = "degraded"
	} else {
		h.Database.Reachable = true
		h.Database.SchemaVersion = version
	}

	h.Secrets.Backend = s.deps.SecretsBackend
	if s.deps.SecretsBackend == "" {
		h.Warnings = append(h.Warnings, "no credential key custody is configured")
		h.Status = "degraded"
	}

	configured, err := s.deps.Auth.Configured(ctx)
	switch {
	case err != nil:
		h.Warnings = append(h.Warnings, "could not determine whether an API token is configured")
		h.Status = "degraded"
	case !configured:
		// Reachable only by an authenticated caller, so this cannot help an
		// attacker — but it is exactly what an operator debugging a 401 needs.
		h.Warnings = append(h.Warnings, "no API token configured; run `tumika token rotate`")
		h.Status = "degraded"
	default:
		h.Auth.TokenConfigured = true
	}

	return h
}
