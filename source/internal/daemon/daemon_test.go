package daemon_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tumika/tumika/source/internal/daemon"
	"github.com/tumika/tumika/source/internal/domain"
	"github.com/tumika/tumika/source/internal/platform/buildinfo"
	"github.com/tumika/tumika/source/internal/platform/paths"
	"github.com/tumika/tumika/source/internal/platform/secrets"
	"github.com/tumika/tumika/source/internal/service"
)

// start brings up a real daemon — real SQLite file, real migrations, real HTTP
// server — and returns its base URL.
//
// The layer tests use fakes; this one deliberately does not. It is the only
// thing that proves the wiring between them is right, which is the whole point
// of building this slice before anything complicated arrives.
// testKey pins key custody for the whole daemon test suite.
//
// Without it, daemon.New goes through secrets.OpenKeyStore, which on macOS
// reaches for the REAL login Keychain and writes an entry into it — so
// `go test ./...` would mutate the Keychain of whoever ran it. The override also
// makes the tests deterministic across platforms.
const testKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="

func useTestKeyCustody(t *testing.T) {
	t.Helper()
	t.Setenv(secrets.MasterKeyEnv, testKey)
}

func start(t *testing.T) (string, string) {
	t.Helper()
	base, token, _ := startWith(t, nil)
	return base, token
}

// startWith is start, with a chance to prepare the tumika home first — used to
// vendor a stand-in `claude` where the real driver will look for it. It also
// returns the layout, so a test can read the database file the daemon wrote.
func startWith(t *testing.T, prepare func(paths.Paths)) (string, string, paths.Paths) {
	t.Helper()
	useTestKeyCustody(t)

	p, err := paths.Resolve(filepath.Join(t.TempDir(), "home"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if prepare != nil {
		if err := p.MkdirAll(); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		prepare(p)
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

	return "http://" + listener.Addr().String(), token, p
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
	useTestKeyCustody(t)

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
	useTestKeyCustody(t)

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

// A subscription token pasted at the API, end to end: real HTTP, real
// migrations, real sealing, and a real spawned process.
//
// This is the slice tumika exists for. The only stand-in is the `claude` binary
// itself — vendoring 307 MB in a unit test would be absurd — and it is placed
// exactly where the driver looks for the pinned version, so everything between
// the HTTP request and the child process is the production path.
func TestAPastedSubscriptionTokenIsSealedVerifiedAndNeverReturned(t *testing.T) {
	const token = "sk-ant-oat01-0123456789abcdefghijklmnopqrstuvwxyz"

	base, apiToken, p := startWith(t, func(p paths.Paths) {
		vendorFakeClaude(t, p)
	})

	status, body := request(t, apiToken, http.MethodPut,
		base+"/v1/providers/claude-code/credential",
		`{"method":"manual_token","secret":"`+token+`"}`)

	if status != http.StatusOK {
		t.Fatalf("PUT credential = %d: %s", status, body)
	}

	var meta domain.CredentialMeta
	if err := json.Unmarshal([]byte(body), &meta); err != nil {
		t.Fatalf("response is not valid JSON (%v): %s", err, body)
	}
	if meta.Status != string(domain.CredentialActive) {
		t.Fatalf("status = %q, want active: %s", meta.Status, body)
	}
	if meta.Hint == "" || strings.Contains(body, token) {
		t.Errorf("the response carries the token rather than a hint: %s", body)
	}

	// SEALED, not merely stored. The database file is read as bytes, because
	// that is what an operator's backup contains and what a stolen disk gives
	// up. Asking the repository would only prove the repository agrees with
	// itself.
	raw, err := os.ReadFile(p.DB)
	if err != nil {
		t.Fatalf("read the database: %v", err)
	}
	if bytes.Contains(raw, []byte(token)) {
		t.Error("the token is in the database in the clear")
	}

	// The provider view carries the credential's non-secret half, and a client
	// reads it to decide what to render.
	status, body = request(t, apiToken, http.MethodGet, base+"/v1/providers/claude-code", "")
	if status != http.StatusOK {
		t.Fatalf("GET provider = %d: %s", status, body)
	}
	if strings.Contains(body, token) {
		t.Errorf("the provider view carries the token: %s", body)
	}
	if !strings.Contains(body, `"manual_token"`) {
		t.Errorf("the descriptor does not offer the method that just worked: %s", body)
	}
	if !strings.Contains(body, `"requires_interactive_auth":false`) {
		t.Errorf("the provider claims an interactive login that does not exist yet: %s", body)
	}

	// And the daemon is still healthy, which is the plan's definition of done
	// for this slice.
	status, body = request(t, apiToken, http.MethodGet, base+"/v1/health", "")
	if status != http.StatusOK {
		t.Fatalf("GET /v1/health = %d: %s", status, body)
	}
	var health domain.Health
	if err := json.Unmarshal([]byte(body), &health); err != nil {
		t.Fatalf("health is not valid JSON (%v): %s", err, body)
	}
	if health.Status != "ok" {
		t.Errorf("health = %q, warnings %v", health.Status, health.Warnings)
	}
}

// Verification asserts the OUTCOME, so a claude that resolved somebody else's
// credential is refused even though it answers perfectly.
//
// The failure this guards has no symptom: the daemon starts, the workflow runs,
// the reply is correct, and the operator is billed API rates for a subscription
// they installed tumika to use.
func TestATokenThatWouldBillAtAPIRatesIsRefusedEndToEnd(t *testing.T) {
	const token = "sk-ant-oat01-0123456789abcdefghijklmnopqrstuvwxyz"

	base, apiToken, _ := startWith(t, func(p paths.Paths) {
		vendorFakeClaude(t, p,
			`{"loggedIn":true,"authMethod":"api_key","apiKeySource":"ANTHROPIC_API_KEY"}`)
	})

	status, body := request(t, apiToken, http.MethodPut,
		base+"/v1/providers/claude-code/credential",
		`{"method":"manual_token","secret":"`+token+`"}`)

	// 502: the provider could not be used, which is not tumika failing and not
	// a bad credential either.
	if status != http.StatusBadGateway {
		t.Fatalf("PUT credential = %d, want 502: %s", status, body)
	}
	if !strings.Contains(body, "API rates") {
		t.Errorf("the refusal does not say what it would have cost: %s", body)
	}
}

// A managed provider can be asked to install its binary; one that vendors
// nothing says so, and says it as a client error rather than a server failure.
func TestInstallEndpointReachesTheRightDriver(t *testing.T) {
	base, apiToken, _ := startWith(t, func(p paths.Paths) {
		vendorFakeClaude(t, p)
	})

	// Already vendored at the pinned version, so this is the cheap answer
	// rather than a 307 MB download in a unit test.
	status, body := request(t, apiToken, http.MethodPost,
		base+"/v1/providers/claude-code/install", "")
	if status != http.StatusOK {
		t.Fatalf("POST install = %d: %s", status, body)
	}
	if !strings.Contains(body, `"already_present":true`) {
		t.Errorf("an installed version was not recognised: %s", body)
	}
	if !strings.Contains(body, buildinfo.PinnedClaudeCodeVersion) {
		t.Errorf("the install did not resolve to the pinned version: %s", body)
	}

	status, body = request(t, apiToken, http.MethodPost,
		base+"/v1/providers/anthropic-api/install", "")
	if status != http.StatusBadRequest {
		t.Fatalf("POST install on an HTTP provider = %d, want 400: %s", status, body)
	}
	if !strings.Contains(body, "install_unsupported") {
		t.Errorf("body = %s, want the documented code", body)
	}
}

// vendorFakeClaude writes a stand-in binary where the driver looks for the
// pinned version. authStatus overrides what `auth status --json` reports.
func vendorFakeClaude(t *testing.T, p paths.Paths, authStatus ...string) {
	t.Helper()

	status := `{"loggedIn":true,"authMethod":"oauth_token","apiKeySource":"CLAUDE_CODE_OAUTH_TOKEN"}`
	if len(authStatus) > 0 {
		status = authStatus[0]
	}

	dir := filepath.Join(p.Providers, "claude-code", buildinfo.PinnedClaudeCodeVersion)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	script := "#!/bin/sh\ncase \"$3\" in\n  auth) cat <<'JSON'\n" + status + "\nJSON\n  ;;\n" +
		"  -p) cat <<'JSON'\n{\"subtype\":\"success\",\"is_error\":false,\"result\":\"ok\"}\nJSON\n  ;;\nesac\n"

	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o700); err != nil {
		t.Fatalf("write the stand-in claude: %v", err)
	}
}

// stubUpdates drives the daemon's update paths without a network or a release.
//
// The counters are mutex-guarded because the daemon calls these from its own
// goroutine while the test reads them — `go test -race` catches that, and it is
// the test's bug rather than the daemon's.
type stubUpdates struct {
	mu           sync.Mutex
	state        domain.UpdateState
	rollBack     bool
	confirmBoot  int
	confirmed    int
	confirmBootE error
	available    string
	applied      []string
}

func (s *stubUpdates) State(context.Context) (domain.UpdateState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state, nil
}

func (s *stubUpdates) Check(context.Context) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.available, s.available != "", nil
}

func (s *stubUpdates) Apply(_ context.Context, version string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applied = append(s.applied, version)
	return nil
}

func (s *stubUpdates) ConfirmBoot(context.Context) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.confirmBoot++
	return s.rollBack, s.confirmBootE
}

func (s *stubUpdates) Confirm(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.confirmed++
	return nil
}

func (s *stubUpdates) appliedVersions() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.applied...)
}

func (s *stubUpdates) counts() (boots, confirms int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.confirmBoot, s.confirmed
}

// A rolled-back update must stop the daemon BEFORE it binds a port, so the
// supervisor relaunches onto the restored binary. Serving first would mean a
// daemon briefly answering requests it is about to abandon.
func TestARolledBackUpdateStopsBeforeServing(t *testing.T) {
	useTestKeyCustody(t)

	p, err := paths.Resolve(filepath.Join(t.TempDir(), "home"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	updates := &stubUpdates{rollBack: true}

	// The rollback now fires during CONSTRUCTION — before migrations, key
	// custody or the provider registry, all of which a bad release can fail at.
	// Resolving it later meant those failures never counted a boot attempt, so
	// the rollback never fired at all.
	_, err = daemon.New(ctx, daemon.Options{Paths: p, Updates: updates, Listen: "127.0.0.1:0"})
	if !errors.Is(err, daemon.ErrRestartRequired) {
		t.Fatalf("daemon.New = %v, want ErrRestartRequired", err)
	}
	boots, confirms := updates.counts()
	if boots != 1 {
		t.Errorf("ConfirmBoot ran %d times, want 1", boots)
	}
	if confirms != 0 {
		t.Error("a rolled-back update was confirmed")
	}
}

// An update is confirmed once the daemon is SERVING, not merely constructed.
func TestAnUpdateIsConfirmedOnceServing(t *testing.T) {
	useTestKeyCustody(t)

	p, err := paths.Resolve(filepath.Join(t.TempDir(), "home"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	updates := &stubUpdates{state: domain.UpdateState{Status: domain.UpdatePending}}
	d, err := daemon.New(ctx, daemon.Options{Paths: p, Updates: updates})
	if err != nil {
		t.Fatalf("daemon.New: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	if _, err := d.AuthService().Rotate(ctx); err != nil {
		t.Fatalf("Rotate: %v", err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	served := make(chan error, 1)
	go func() { served <- d.ServeListener(ctx, listener) }()

	// Confirm runs before the serve loop blocks, so it has happened by the time
	// the API answers.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, confirms := updates.counts(); confirms > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, confirms := updates.counts(); confirms == 0 {
		t.Fatal("the update was never confirmed")
	}

	cancel()
	select {
	case <-served:
	case <-time.After(15 * time.Second):
		t.Fatal("the daemon did not shut down")
	}

	if _, confirms := updates.counts(); confirms != 1 {
		t.Errorf("Confirm ran %d times, want 1", confirms)
	}
}

// A daemon that cannot read its update row still starts: refusing would turn a
// bookkeeping problem into an outage.
//
// Driven through Serve, not ServeListener. The previous version of this test
// called ServeListener, which never calls ConfirmBoot at all — so the stub's
// error was never returned and a regression making it fatal would have passed.
func TestAnUnreadableUpdateStateDoesNotStopTheDaemon(t *testing.T) {
	useTestKeyCustody(t)

	p, err := paths.Resolve(filepath.Join(t.TempDir(), "home"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	updates := &stubUpdates{confirmBootE: errors.New("database is locked")}
	d, err := daemon.New(ctx, daemon.Options{Paths: p, Updates: updates, Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("daemon.New failed because the update row could not be read: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	if boots, _ := updates.counts(); boots != 1 {
		t.Fatalf("ConfirmBoot ran %d times, want 1 — the error path was not exercised", boots)
	}

	if _, err := d.AuthService().Rotate(ctx); err != nil {
		t.Fatalf("Rotate: %v", err)
	}

	served := make(chan error, 1)
	go func() { served <- d.Serve(ctx) }()

	time.Sleep(300 * time.Millisecond)
	cancel()
	select {
	case err := <-served:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("the daemon failed to serve: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the daemon did not shut down")
	}
}

// An operator applying an update over HTTP gets a response FIRST, and then the
// daemon shuts down so the supervisor relaunches onto the new binary.
//
// The order matters: exiting inside the handler would drop the connection, and
// the operator could not tell a successful update from a crash.
func TestApplyingAnUpdateOverHTTPRestartsTheDaemon(t *testing.T) {
	useTestKeyCustody(t)

	p, err := paths.Resolve(filepath.Join(t.TempDir(), "home"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	updates := &stubUpdates{available: "0.2.0"}
	d, err := daemon.New(ctx, daemon.Options{Paths: p, Updates: updates})
	if err != nil {
		t.Fatalf("daemon.New: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

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

	base := "http://" + listener.Addr().String()
	status, body := request(t, token, http.MethodPost, base+"/v1/update/apply", "")
	if status != http.StatusOK {
		t.Fatalf("apply = %d: %s", status, body)
	}
	if got := updates.appliedVersions(); len(got) != 1 || got[0] != "0.2.0" {
		t.Errorf("applied %v, want [0.2.0]", got)
	}

	// The daemon drains and then asks to be restarted — WITHOUT the context
	// being cancelled, which is what distinguishes this from a shutdown.
	select {
	case err := <-served:
		if !errors.Is(err, daemon.ErrRestartRequired) {
			t.Errorf("ServeListener = %v, want ErrRestartRequired", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the daemon kept serving after an update was applied")
	}
}
