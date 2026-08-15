package service

import (
	"context"
	"time"

	"github.com/tumika/tumika/source/internal/domain"
)

// SchemaVersionFunc reports the database's migration version.
//
// A function rather than an interface because it is one call, and taking the
// store itself would put a concrete repository implementation in a service's
// dependencies — which depguard forbids, and rightly: it is what would stop
// HealthService being testable without a database.
type SchemaVersionFunc func(ctx context.Context) (int64, error)

// HealthService assembles the daemon's self-report.
type HealthService interface {
	Snapshot(ctx context.Context) domain.Health
}

type healthService struct {
	version string
	started time.Time
	schema  SchemaVersionFunc
	auth    AuthService
	now     func() time.Time
}

// NewHealthService builds the service. started is the process start time, so
// uptime survives however long the first request takes to arrive.
func NewHealthService(version string, started time.Time, schema SchemaVersionFunc, auth AuthService) HealthService {
	return &healthService{
		version: version,
		started: started,
		schema:  schema,
		auth:    auth,
		now:     time.Now,
	}
}

// Snapshot never returns an error.
//
// A health check that fails to report is the least useful failure mode there is:
// the caller learns nothing except that something is wrong somewhere. Each
// component's failure is captured in the report instead, and degrades Status.
func (s *healthService) Snapshot(ctx context.Context) domain.Health {
	h := domain.Health{
		Status:  "ok",
		Version: s.version,
		Started: s.started,
		Uptime:  s.now().Sub(s.started).Round(time.Second).String(),
	}

	if version, err := s.schema(ctx); err != nil {
		h.Database.Error = err.Error()
		h.Warnings = append(h.Warnings, "database is unreachable")
		h.Status = "degraded"
	} else {
		h.Database.Reachable = true
		h.Database.SchemaVersion = version
	}

	configured, err := s.auth.Configured(ctx)
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
