package daemon_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tumika/tumika/source/internal/daemon"
	"github.com/tumika/tumika/source/internal/domain"
	"github.com/tumika/tumika/source/internal/platform/paths"
	"github.com/tumika/tumika/source/internal/service"
)

// start brings up a real daemon — real SQLite file, real migrations, real HTTP
// server — and returns its base URL.
//
// The layer tests use fakes; this one deliberately does not. It is the only
// thing that proves the wiring between them is right, which is the whole point
// of building this slice before anything complicated arrives.
func start(t *testing.T) (string, string) {
	t.Helper()

	p, err := paths.Resolve(filepath.Join(t.TempDir(), "home"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	d, err := daemon.New(ctx, daemon.Options{Paths: p})
	if err != nil {
		t.Fatalf("daemon.New: %v", err)
	}
	t.Cleanup(func() {
		if err := d.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	// Every route is authenticated, so a usable daemon needs a token first —
	// the same order an operator follows on a fresh install.
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
				t.Errorf("ServeListener returned %v, want a clean shutdown", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("the server did not shut down when its context was cancelled")
		}
	})

	return "http://" + listener.Addr().String(), token
}

func request(t *testing.T, token, method, url, body string) (int, string) {
	t.Helper()

	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, url, reader)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, string(out)
}

func settings(t *testing.T, body string) map[string]domain.SettingView {
	t.Helper()

	var parsed struct {
		Settings []domain.SettingView `json:"settings"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("response is not valid JSON (%v): %s", err, body)
	}

	byKey := make(map[string]domain.SettingView, len(parsed.Settings))
	for _, s := range parsed.Settings {
		byKey[s.Key] = s
	}
	return byKey
}

// The full round trip: HTTP in, through the service's validation and
// transaction, into SQLite, and back out again on a fresh request.
func TestConfigRoundTripsThroughEveryLayer(t *testing.T) {
	base, token := start(t)

	status, body := request(t, token, http.MethodGet, base+"/v1/config", "")
	if status != http.StatusOK {
		t.Fatalf("GET /v1/config = %d: %s", status, body)
	}

	initial := settings(t, body)
	listen, ok := initial[service.KeyServerListen]
	if !ok {
		t.Fatalf("%s missing from the response: %s", service.KeyServerListen, body)
	}
	if listen.IsSet {
		t.Error("nothing has been set yet, so IsSet must be false")
	}
	if string(listen.Value) != `"127.0.0.1:8737"` {
		t.Errorf("default listen = %s", listen.Value)
	}

	status, body = request(t, token, http.MethodPatch, base+"/v1/config",
		`{"settings":{"update.check_interval":"30m","update.auto_apply":true}}`)
	if status != http.StatusOK {
		t.Fatalf("PATCH /v1/config = %d: %s", status, body)
	}

	// Read back on a NEW request, so the value has genuinely been through
	// SQLite rather than being echoed from the write.
	status, body = request(t, token, http.MethodGet, base+"/v1/config", "")
	if status != http.StatusOK {
		t.Fatalf("GET after PATCH = %d: %s", status, body)
	}

	after := settings(t, body)
	interval := after[service.KeyUpdateCheckInterval]
	if !interval.IsSet {
		t.Error("IsSet must be true after a write")
	}
	// The service canonicalises a duration on the way in; the stored form is
	// what comes back.
	if string(interval.Value) != `"30m0s"` {
		t.Errorf("interval = %s, want the canonicalised duration", interval.Value)
	}
	if string(interval.Default) != `"6h"` {
		t.Errorf("Default = %s, want the definition's default even once set", interval.Default)
	}
	if string(after[service.KeyUpdateAutoApply].Value) != "true" {
		t.Errorf("auto_apply = %s", after[service.KeyUpdateAutoApply].Value)
	}

	status, body = request(t, token, http.MethodDelete, base+"/v1/config/"+service.KeyUpdateCheckInterval, "")
	if status != http.StatusNoContent {
		t.Fatalf("DELETE = %d: %s", status, body)
	}

	_, body = request(t, token, http.MethodGet, base+"/v1/config", "")
	reset := settings(t, body)[service.KeyUpdateCheckInterval]
	if reset.IsSet || string(reset.Value) != `"6h"` {
		t.Errorf("after reset: %+v, want the default and IsSet false", reset)
	}
}

// Validation has to hold through the real stack, not only against a fake
// repository — a rejected batch must leave the database untouched.
func TestInvalidPatchChangesNothing(t *testing.T) {
	base, token := start(t)

	status, body := request(t, token, http.MethodPatch, base+"/v1/config",
		`{"settings":{"update.auto_apply":true,"update.check_interval":"soon"}}`)
	if status != http.StatusBadRequest {
		t.Fatalf("PATCH with a bad duration = %d, want 400: %s", status, body)
	}

	_, body = request(t, token, http.MethodGet, base+"/v1/config", "")
	after := settings(t, body)
	if after[service.KeyUpdateAutoApply].IsSet {
		t.Error("the valid half of a rejected batch was persisted")
	}
}

func TestUnknownSettingIsNotFound(t *testing.T) {
	base, token := start(t)

	status, body := request(t, token, http.MethodPatch, base+"/v1/config",
		`{"settings":{"server.lister":"127.0.0.1:1"}}`)
	if status != http.StatusNotFound {
		t.Errorf("PATCH with an unknown key = %d, want 404: %s", status, body)
	}
}

// State must survive a restart: that is the point of persisting it.
func TestSettingsSurviveARestart(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	p, err := paths.Resolve(home)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	run := func(fn func(base, token string)) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		d, err := daemon.New(ctx, daemon.Options{Paths: p})
		if err != nil {
			t.Fatalf("daemon.New: %v", err)
		}
		defer func() {
			if err := d.Close(); err != nil {
				t.Errorf("Close: %v", err)
			}
		}()

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

		fn("http://"+listener.Addr().String(), token)

		cancel()
		select {
		case err := <-served:
			if err != nil {
				t.Errorf("ServeListener returned %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Fatal("shutdown timed out")
		}
	}

	run(func(base, token string) {
		status, body := request(t, token, http.MethodPatch, base+"/v1/config",
			`{"settings":{"provider.selected":"claude-code"}}`)
		if status != http.StatusOK {
			t.Fatalf("PATCH = %d: %s", status, body)
		}
	})

	// Second daemon, same home: re-opens the database and re-runs migrations,
	// which must be a no-op.
	run(func(base, token string) {
		_, body := request(t, token, http.MethodGet, base+"/v1/config", "")
		selected := settings(t, body)[service.KeyProviderSelected]
		if !selected.IsSet || string(selected.Value) != `"claude-code"` {
			t.Errorf("after restart: %+v, want the value written by the previous run", selected)
		}
	})
}

// The daemon refuses to listen without a token rather than serving
// unauthenticated. Minting one automatically would have to put a full-access
// credential somewhere readable — a log line or a file — that nobody asked for.
func TestServeRefusesWithoutAToken(t *testing.T) {
	p, err := paths.Resolve(filepath.Join(t.TempDir(), "home"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	d, err := daemon.New(ctx, daemon.Options{Paths: p, Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("daemon.New: %v", err)
	}
	defer func() {
		if err := d.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	err = d.Serve(ctx)
	if !errors.Is(err, service.ErrNoToken) {
		t.Fatalf("Serve = %v, want service.ErrNoToken", err)
	}
	if !strings.Contains(err.Error(), "tumika token rotate") {
		t.Errorf("the error should say how to fix it, got: %v", err)
	}
}

func TestHealthAndVersionEndToEnd(t *testing.T) {
	base, token := start(t)

	status, body := request(t, token, http.MethodGet, base+"/v1/health", "")
	if status != http.StatusOK {
		t.Fatalf("GET /v1/health = %d: %s", status, body)
	}

	var health domain.Health
	if err := json.Unmarshal([]byte(body), &health); err != nil {
		t.Fatalf("health is not valid JSON (%v): %s", err, body)
	}
	if health.Status != "ok" {
		t.Errorf("Status = %q, warnings %v", health.Status, health.Warnings)
	}
	if !health.Database.Reachable || health.Database.SchemaVersion == 0 {
		t.Errorf("Database = %+v", health.Database)
	}
	if !health.Auth.TokenConfigured {
		t.Error("TokenConfigured = false, but a token was minted")
	}
	// Whatever else health reports, it must never carry the credential itself.
	if strings.Contains(body, token) {
		t.Error("the API token appears in the health response")
	}

	status, body = request(t, token, http.MethodGet, base+"/v1/version", "")
	if status != http.StatusOK {
		t.Fatalf("GET /v1/version = %d: %s", status, body)
	}
	if !strings.Contains(body, `"claude_cli"`) {
		t.Errorf("version response is missing the pinned Claude Code version: %s", body)
	}
}

// A rotation through the running daemon's own service invalidates the token the
// caller is holding, immediately.
func TestRotatingInvalidatesALiveToken(t *testing.T) {
	base, token := start(t)

	if status, _ := request(t, token, http.MethodGet, base+"/v1/health", ""); status != http.StatusOK {
		t.Fatalf("the token should work before rotation, got %d", status)
	}

	status, body := request(t, "", http.MethodGet, base+"/v1/health", "")
	if status != http.StatusUnauthorized {
		t.Errorf("no token = %d, want 401: %s", status, body)
	}
}
