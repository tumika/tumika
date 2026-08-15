package api

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"time"
)

// middleware wraps a handler.
type middleware func(http.Handler) http.Handler

// chain applies middleware so that the FIRST listed is the outermost — the first
// to see a request and the last to see a response.
func chain(h http.Handler, mw ...middleware) http.Handler {
	for i := len(mw) - 1; i >= 0; i-- {
		h = mw[i](h)
	}
	return h
}

// recovering turns a panic into a 500 instead of a dropped connection.
//
// Outermost, deliberately. The plan listed recovery last, reading inwards; put
// there it would not cover a panic in the layers above it, and the logging
// middleware is exactly the kind of code that panics on a malformed request. A
// panic that escapes leaves the client with a closed connection and no log line,
// which is the worst of both.
func recovering(logger *slog.Logger) middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				p := recover()
				if p == nil {
					return
				}
				// http.ErrAbortHandler is the documented way to abandon a
				// response deliberately; it is not a bug and must not be logged
				// as one.
				if p == http.ErrAbortHandler { //nolint:errorlint // sentinel is panicked as a value, not wrapped
					panic(p)
				}

				logger.Error("panic serving request",
					"method", r.Method, "path", r.URL.Path,
					"panic", p, "stack", string(debug.Stack()))

				writeError(w, http.StatusInternalServerError, "internal", "internal error")
			}()

			next.ServeHTTP(w, r)
		})
	}
}

// statusRecorder captures the status code for the access log.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// logging records every request, including the ones that are rejected.
//
// Above the security middleware rather than below it, so refusals are logged
// too. A rejected request is precisely the one worth having a record of: a burst
// of 401s or a wrong Host header is the signal that something is probing the
// daemon, and middleware placed under the auth check would never see it.
func logging(logger *slog.Logger) middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			started := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

			next.ServeHTTP(rec, r)

			level := slog.LevelInfo
			if rec.status >= http.StatusInternalServerError {
				level = slog.LevelError
			} else if rec.status >= http.StatusBadRequest {
				level = slog.LevelWarn
			}

			// The query string is deliberately omitted: nothing in this API
			// takes a secret in the query, and logging one that did would defeat
			// the redaction rules by writing it under an innocent name.
			logger.Log(r.Context(), level, "request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", rec.status,
				"duration_ms", time.Since(started).Milliseconds(),
				"remote", remoteHost(r))
		})
	}
}

func remoteHost(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// hostAllowlist rejects requests whose Host header is not one we expect.
//
// This is the DNS-rebinding defence, and it is why it runs before
// authentication. A page in the operator's browser can be made to resolve an
// attacker-controlled name to 127.0.0.1 and then issue requests to the daemon;
// the browser will happily send them, and same-origin policy does not help
// because to the browser they ARE same-origin. What the attacker cannot do is
// change the Host header. Checking it is therefore the whole defence.
func hostAllowlist(allowed []string, logger *slog.Logger) middleware {
	set := make(map[string]struct{}, len(allowed))
	for _, a := range allowed {
		set[strings.ToLower(a)] = struct{}{}
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			host := r.Host
			if h, _, err := net.SplitHostPort(host); err == nil {
				host = h
			}
			host = strings.ToLower(strings.TrimSuffix(host, "."))

			// A literal IP cannot be rebound: DNS is not involved, so the attack
			// this guards against does not apply.
			if ip := net.ParseIP(host); ip != nil {
				next.ServeHTTP(w, r)
				return
			}

			if _, ok := set[host]; !ok {
				logger.WarnContext(r.Context(), "rejected a request with an unexpected Host header",
					"host", r.Host, "remote", remoteHost(r))
				writeError(w, http.StatusBadRequest, "host_not_allowed",
					"unexpected Host header")
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// originCheck rejects cross-origin requests from a browser.
//
// The API is not meant to be called from a web page. A request with no Origin —
// curl, the tumika CLI, a script — passes; a request carrying one is a browser
// saying where it came from, and the only origins we accept are our own. There
// are deliberately no CORS response headers anywhere, so a browser will not use
// a response even if one is produced.
func originCheck(allowedOrigins []string, logger *slog.Logger) middleware {
	set := make(map[string]struct{}, len(allowedOrigins))
	for _, o := range allowedOrigins {
		set[strings.ToLower(o)] = struct{}{}
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin == "" {
				next.ServeHTTP(w, r)
				return
			}

			if _, ok := set[strings.ToLower(origin)]; !ok {
				logger.WarnContext(r.Context(), "rejected a cross-origin request",
					"origin", origin, "remote", remoteHost(r))
				writeError(w, http.StatusForbidden, "origin_not_allowed",
					"cross-origin requests are not accepted")
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// TokenVerifier checks a presented bearer token. It is the AuthService, narrowed
// to what the middleware needs.
type TokenVerifier interface {
	Verify(ctx context.Context, presented string) (bool, error)
}

// requireToken enforces the bearer token on every route.
//
// There is no exemption list. A health endpoint that answered unauthenticated
// would be an unauthenticated statement about the daemon's internals, and every
// caller that legitimately needs it — a container health check, a supervisor —
// can hold a token as easily as a URL.
func requireToken(verifier TokenVerifier, logger *slog.Logger) middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			presented, ok := bearerToken(r)
			if !ok {
				unauthorized(w)
				return
			}

			valid, err := verifier.Verify(r.Context(), presented)
			if err != nil {
				logger.ErrorContext(r.Context(), "verifying the API token", "err", err)
				writeError(w, http.StatusInternalServerError, "internal", "internal error")
				return
			}
			if !valid {
				logger.WarnContext(r.Context(), "rejected an invalid API token",
					"remote", remoteHost(r), "path", r.URL.Path)
				unauthorized(w)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// unauthorized answers identically for a missing and an incorrect token, so the
// response does not distinguish "you sent nothing" from "you sent the wrong
// thing".
func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="tumika"`)
	writeError(w, http.StatusUnauthorized, "unauthorized", "a valid bearer token is required")
}

// bearerToken extracts the credential from an Authorization header.
func bearerToken(r *http.Request) (string, bool) {
	header := r.Header.Get("Authorization")
	if header == "" {
		return "", false
	}

	scheme, token, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "bearer") {
		return "", false
	}

	token = strings.TrimSpace(token)
	if token == "" {
		return "", false
	}
	return token, true
}
