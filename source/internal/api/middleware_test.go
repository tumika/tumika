package api_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tumika/tumika/source/internal/api"
)

// verifier accepts exactly one token, so tests can distinguish "no token",
// "wrong token" and "right token".
type verifier struct {
	valid string
	err   error
}

func (v verifier) Verify(_ context.Context, presented string) (bool, error) {
	if v.err != nil {
		return false, v.err
	}
	return presented == v.valid, nil
}

const goodToken = "tmk_the-right-one"

func router(t *testing.T, auth api.TokenVerifier, logs io.Writer, origins []string) http.Handler {
	t.Helper()
	if logs == nil {
		logs = io.Discard
	}
	return api.NewRouter(api.Deps{
		Config:         &fakeConfigService{},
		Health:         stubHealth{},
		Auth:           auth,
		Logger:         slog.New(slog.NewJSONHandler(logs, nil)),
		AllowedHosts:   []string{"localhost"},
		AllowedOrigins: origins,
	})
}

func req(method, url string) *http.Request {
	return httptest.NewRequest(method, url, nil)
}

func TestBearerTokenIsRequired(t *testing.T) {
	tests := []struct {
		name   string
		header string
		want   int
	}{
		{"no Authorization header", "", http.StatusUnauthorized},
		{"empty bearer", "Bearer ", http.StatusUnauthorized},
		{"wrong token", "Bearer tmk_wrong", http.StatusUnauthorized},
		{"wrong scheme", "Basic " + goodToken, http.StatusUnauthorized},
		{"token without a scheme", goodToken, http.StatusUnauthorized},
		{"correct token", "Bearer " + goodToken, http.StatusOK},
		{"scheme is case-insensitive", "bearer " + goodToken, http.StatusOK},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := req(http.MethodGet, "http://127.0.0.1:8737/v1/health")
			if tc.header != "" {
				r.Header.Set("Authorization", tc.header)
			}

			rec := httptest.NewRecorder()
			router(t, verifier{valid: goodToken}, nil, nil).ServeHTTP(rec, r)

			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d: %s", rec.Code, tc.want, rec.Body)
			}
		})
	}
}

// Every route is behind the token. An unauthenticated health endpoint would be
// an unauthenticated statement about the daemon's internals.
func TestNoRouteIsExemptFromAuthentication(t *testing.T) {
	for _, target := range []string{"/v1/health", "/v1/version", "/v1/config"} {
		t.Run(target, func(t *testing.T) {
			rec := httptest.NewRecorder()
			router(t, verifier{valid: goodToken}, nil, nil).
				ServeHTTP(rec, req(http.MethodGet, "http://127.0.0.1:8737"+target))

			if rec.Code != http.StatusUnauthorized {
				t.Errorf("%s without a token = %d, want 401", target, rec.Code)
			}
		})
	}
}

// A missing token and a wrong one must be indistinguishable in the response.
func TestUnauthorizedResponsesDoNotDistinguishMissingFromWrong(t *testing.T) {
	get := func(header string) *httptest.ResponseRecorder {
		r := req(http.MethodGet, "http://127.0.0.1:8737/v1/health")
		if header != "" {
			r.Header.Set("Authorization", header)
		}
		rec := httptest.NewRecorder()
		router(t, verifier{valid: goodToken}, nil, nil).ServeHTTP(rec, r)
		return rec
	}

	missing, wrong := get(""), get("Bearer tmk_wrong")

	if missing.Body.String() != wrong.Body.String() {
		t.Errorf("bodies differ:\n missing: %s\n wrong:   %s", missing.Body, wrong.Body)
	}
	if missing.Header().Get("WWW-Authenticate") != wrong.Header().Get("WWW-Authenticate") {
		t.Error("WWW-Authenticate differs between a missing and a wrong token")
	}
}

// The DNS-rebinding defence. An attacker can point a name they control at
// 127.0.0.1 and have a browser issue same-origin requests; what they cannot do
// is forge the Host header.
func TestHostAllowlist(t *testing.T) {
	tests := []struct {
		name string
		host string
		want int
	}{
		{"loopback IP", "127.0.0.1:8737", http.StatusOK},
		{"IPv6 loopback", "[::1]:8737", http.StatusOK},
		{"localhost", "localhost:8737", http.StatusOK},
		{"localhost without a port", "localhost", http.StatusOK},
		{"attacker-controlled name", "rebind.evil.example:8737", http.StatusBadRequest},
		{"bare evil name", "evil.example", http.StatusBadRequest},
		{"trailing dot is still evil", "evil.example.:8737", http.StatusBadRequest},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := req(http.MethodGet, "http://127.0.0.1:8737/v1/health")
			r.Host = tc.host
			r.Header.Set("Authorization", "Bearer "+goodToken)

			rec := httptest.NewRecorder()
			router(t, verifier{valid: goodToken}, nil, nil).ServeHTTP(rec, r)

			if rec.Code != tc.want {
				t.Errorf("Host %q = %d, want %d: %s", tc.host, rec.Code, tc.want, rec.Body)
			}
		})
	}
}

// The Host check runs before authentication, so an unauthenticated probe with a
// rebound name is turned away on the Host rather than on the token — which is
// the whole point of putting it first.
func TestHostIsCheckedBeforeAuthentication(t *testing.T) {
	r := req(http.MethodGet, "http://127.0.0.1:8737/v1/health")
	r.Host = "rebind.evil.example"

	rec := httptest.NewRecorder()
	router(t, verifier{valid: goodToken}, nil, nil).ServeHTTP(rec, r)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 — the Host check must run before the token check", rec.Code)
	}
}

func TestOriginCheck(t *testing.T) {
	tests := []struct {
		name    string
		origin  string
		allowed []string
		want    int
	}{
		{"no Origin at all (curl, the CLI)", "", nil, http.StatusOK},
		{"a browser origin, none allowed", "https://evil.example", nil, http.StatusForbidden},
		{"an allowed origin", "http://localhost:3000", []string{"http://localhost:3000"}, http.StatusOK},
		{"a different origin than allowed", "https://evil.example", []string{"http://localhost:3000"}, http.StatusForbidden},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := req(http.MethodGet, "http://127.0.0.1:8737/v1/health")
			r.Header.Set("Authorization", "Bearer "+goodToken)
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}

			rec := httptest.NewRecorder()
			router(t, verifier{valid: goodToken}, nil, tc.allowed).ServeHTTP(rec, r)

			if rec.Code != tc.want {
				t.Errorf("Origin %q = %d, want %d", tc.origin, rec.Code, tc.want)
			}
		})
	}
}

// No CORS headers anywhere: a browser must not be able to use a response even if
// it manages to produce one.
func TestNoCORSHeadersAreEverSet(t *testing.T) {
	r := req(http.MethodGet, "http://127.0.0.1:8737/v1/health")
	r.Header.Set("Authorization", "Bearer "+goodToken)
	r.Header.Set("Origin", "http://localhost:3000")

	rec := httptest.NewRecorder()
	router(t, verifier{valid: goodToken}, nil, []string{"http://localhost:3000"}).ServeHTTP(rec, r)

	for _, header := range []string{
		"Access-Control-Allow-Origin",
		"Access-Control-Allow-Credentials",
		"Access-Control-Allow-Methods",
	} {
		if v := rec.Header().Get(header); v != "" {
			t.Errorf("%s = %q, want it absent", header, v)
		}
	}
}

// Rejections are the requests most worth having a record of: a burst of 401s is
// how a probe looks from the inside.
func TestRejectedRequestsAreLogged(t *testing.T) {
	var logs bytes.Buffer

	rec := httptest.NewRecorder()
	router(t, verifier{valid: goodToken}, &logs, nil).
		ServeHTTP(rec, req(http.MethodGet, "http://127.0.0.1:8737/v1/health"))

	out := logs.String()
	if !strings.Contains(out, `"status":401`) {
		t.Errorf("a rejected request was not logged:\n%s", out)
	}
	if !strings.Contains(out, `"msg":"request"`) {
		t.Errorf("no access-log line was emitted:\n%s", out)
	}
}

// A verifier failure is an internal error, not a rejection: answering 401 would
// tell the caller their token was wrong when we do not know that.
func TestVerifierErrorsAreInternalNotUnauthorized(t *testing.T) {
	r := req(http.MethodGet, "http://127.0.0.1:8737/v1/health")
	r.Header.Set("Authorization", "Bearer "+goodToken)

	rec := httptest.NewRecorder()
	router(t, verifier{err: errors.New("database gone")}, nil, nil).ServeHTTP(rec, r)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "database gone") {
		t.Errorf("internal detail leaked: %s", rec.Body)
	}
}
