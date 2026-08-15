package service_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tumika/tumika/source/internal/service"
)

func TestHealthReportsAHealthyDaemon(t *testing.T) {
	auth, _ := newAuth(t)
	if _, err := auth.Rotate(t.Context()); err != nil {
		t.Fatalf("Rotate: %v", err)
	}

	schema := func(context.Context) (int64, error) { return 1, nil }
	svc := service.NewHealthService("v1.2.3", time.Now().Add(-90*time.Second), schema, auth)

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
	svc := service.NewHealthService("dev", time.Now(), schema, auth)

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
	token, err := auth.Rotate(t.Context())
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}

	schema := func(context.Context) (int64, error) { return 1, nil }
	h := service.NewHealthService("dev", time.Now(), schema, auth).Snapshot(t.Context())

	rendered := strings.Join(append(h.Warnings,
		h.Status, h.Version, h.Uptime, h.Database.Error), " ")
	if strings.Contains(rendered, token) {
		t.Error("the API token appears in the health report")
	}
}
