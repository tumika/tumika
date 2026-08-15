package daemon_test

import (
	"context"
	"encoding/json"
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
func start(t *testing.T) string {
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

	return "http://" + listener.Addr().String()
}

func request(t *testing.T, method, url, body string) (int, string) {
	t.Helper()

	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, url, reader)
	if err != nil {
		t.Fatalf("build request: %v", err)
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
	base := start(t)

	status, body := request(t, http.MethodGet, base+"/v1/config", "")
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

	status, body = request(t, http.MethodPatch, base+"/v1/config",
		`{"settings":{"update.check_interval":"30m","update.auto_apply":true}}`)
	if status != http.StatusOK {
		t.Fatalf("PATCH /v1/config = %d: %s", status, body)
	}

	// Read back on a NEW request, so the value has genuinely been through
	// SQLite rather than being echoed from the write.
	status, body = request(t, http.MethodGet, base+"/v1/config", "")
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

	status, body = request(t, http.MethodDelete, base+"/v1/config/"+service.KeyUpdateCheckInterval, "")
	if status != http.StatusNoContent {
		t.Fatalf("DELETE = %d: %s", status, body)
	}

	_, body = request(t, http.MethodGet, base+"/v1/config", "")
	reset := settings(t, body)[service.KeyUpdateCheckInterval]
	if reset.IsSet || string(reset.Value) != `"6h"` {
		t.Errorf("after reset: %+v, want the default and IsSet false", reset)
	}
}

// Validation has to hold through the real stack, not only against a fake
// repository — a rejected batch must leave the database untouched.
func TestInvalidPatchChangesNothing(t *testing.T) {
	base := start(t)

	status, body := request(t, http.MethodPatch, base+"/v1/config",
		`{"settings":{"update.auto_apply":true,"update.check_interval":"soon"}}`)
	if status != http.StatusBadRequest {
		t.Fatalf("PATCH with a bad duration = %d, want 400: %s", status, body)
	}

	_, body = request(t, http.MethodGet, base+"/v1/config", "")
	after := settings(t, body)
	if after[service.KeyUpdateAutoApply].IsSet {
		t.Error("the valid half of a rejected batch was persisted")
	}
}

func TestUnknownSettingIsNotFound(t *testing.T) {
	base := start(t)

	status, body := request(t, http.MethodPatch, base+"/v1/config",
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

	run := func(fn func(base string)) {
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

		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		served := make(chan error, 1)
		go func() { served <- d.ServeListener(ctx, listener) }()

		fn("http://" + listener.Addr().String())

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

	run(func(base string) {
		status, body := request(t, http.MethodPatch, base+"/v1/config",
			`{"settings":{"provider.selected":"claude-code"}}`)
		if status != http.StatusOK {
			t.Fatalf("PATCH = %d: %s", status, body)
		}
	})

	// Second daemon, same home: re-opens the database and re-runs migrations,
	// which must be a no-op.
	run(func(base string) {
		_, body := request(t, http.MethodGet, base+"/v1/config", "")
		selected := settings(t, body)[service.KeyProviderSelected]
		if !selected.IsSet || string(selected.Value) != `"claude-code"` {
			t.Errorf("after restart: %+v, want the value written by the previous run", selected)
		}
	})
}
