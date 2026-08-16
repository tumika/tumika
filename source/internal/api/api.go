// Package api is layer 1: transport, and nothing else.
//
// A handler decodes the request, calls exactly one service method, and encodes
// the result. It holds no business rules, opens no transactions and does not
// import the repository layer — `depguard` fails the build if it tries. See
// .agents/rules/all-business-logic-lives-in-the-service-layer.md.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/tumika/tumika/source/internal/domain"
	"github.com/tumika/tumika/source/internal/service"
)

// maxBodyBytes caps a request body. The API takes small JSON documents; without
// a cap, an unauthenticated caller could make the daemon buffer whatever it
// liked.
const maxBodyBytes = 256 << 10 // 256 KiB

// Deps are the services the API talks to. Everything here is an interface, so
// handler tests need no database.
type Deps struct {
	Config    service.ConfigService
	Providers service.ProviderService
	Health    service.HealthService
	Auth      TokenVerifier
	Logger    *slog.Logger

	// AllowedHosts are the Host header values accepted for a name-based
	// request. Literal IPs are always accepted — they cannot be rebound.
	AllowedHosts []string
	// AllowedOrigins are the browser origins accepted. Empty means none, which
	// is the intended state: this API is not called from a web page.
	AllowedOrigins []string
}

// NewRouter builds the HTTP handler.
//
// stdlib ServeMux, because Go 1.22+ matches on method and path wildcards and
// this API is about fifteen routes. A framework would earn nothing.
func NewRouter(deps Deps) http.Handler {
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}

	h := &handlers{deps: deps}
	mux := http.NewServeMux()

	mux.HandleFunc("GET /v1/health", h.health)
	mux.HandleFunc("GET /v1/version", h.version)
	mux.HandleFunc("GET /v1/providers", h.listProviders)
	mux.HandleFunc("GET /v1/providers/{id}", h.getProvider)
	mux.HandleFunc("GET /v1/providers/{id}/preflight", h.providerPreflight)
	mux.HandleFunc("POST /v1/providers/{id}/select", h.selectProvider)
	mux.HandleFunc("PUT /v1/providers/{id}/credential", h.putCredential)
	mux.HandleFunc("POST /v1/providers/{id}/verify", h.verifyCredential)
	mux.HandleFunc("DELETE /v1/providers/{id}/credential", h.deleteCredential)

	mux.HandleFunc("GET /v1/config", h.listConfig)
	mux.HandleFunc("PATCH /v1/config", h.patchConfig)
	mux.HandleFunc("DELETE /v1/config/{key}", h.resetConfig)

	// Outermost first. Recovery wraps everything so a panic anywhere becomes a
	// 500; logging sits above the security checks so refusals are recorded;
	// Host runs before Origin and both run before authentication, because an
	// unauthenticated probe is exactly what they exist to turn away.
	return chain(mux,
		recovering(deps.Logger),
		logging(deps.Logger),
		hostAllowlist(deps.AllowedHosts, deps.Logger),
		originCheck(deps.AllowedOrigins, deps.Logger),
		requireToken(deps.Auth, deps.Logger),
	)
}

type handlers struct{ deps Deps }

// errorBody is the single error shape the API returns, everywhere.
type errorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// Configuration and credential metadata must not sit in an intermediary's
	// cache. Set here rather than in middleware so it holds for every response
	// this package writes, including errors.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)

	if v == nil {
		return
	}
	// The status line is already sent, so a failed encode cannot become an error
	// response. Log it and move on.
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Default().Debug("encoding response body", "err", err)
	}
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	var body errorBody
	body.Error.Code = code
	body.Error.Message = message
	writeJSON(w, status, body)
}

// writeServiceError maps a service error onto a status and a code.
//
// This is encoding, not deciding: the service already decided by returning a
// particular sentinel. A handler that inspected domain state to choose a status
// would be making a business decision in the transport layer.
func writeServiceError(w http.ResponseWriter, logger *slog.Logger, err error) {
	switch {
	case errors.Is(err, service.ErrUnknownSetting):
		writeError(w, http.StatusNotFound, "unknown_setting", err.Error())
	case errors.Is(err, service.ErrInvalidSetting):
		writeError(w, http.StatusBadRequest, "invalid_setting", err.Error())
	case errors.Is(err, domain.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, domain.ErrCredentialInvalid):
		writeError(w, http.StatusBadRequest, "credential_invalid", err.Error())
	case errors.Is(err, domain.ErrInteractiveAuthRequired):
		// The mirror of interactive_auth_unsupported: a secret was submitted to
		// a provider that only hands one over through a login session.
		writeError(w, http.StatusBadRequest, "interactive_auth_required", err.Error())
	case errors.Is(err, domain.ErrInteractiveAuthUnsupported):
		writeError(w, http.StatusBadRequest, "interactive_auth_unsupported", err.Error())
	case errors.Is(err, domain.ErrInstallUnsupported):
		writeError(w, http.StatusBadRequest, "install_unsupported", err.Error())
	case errors.Is(err, domain.ErrProviderUnavailable):
		// The provider could not be reached, which is not tumika failing. A 500
		// would tell the operator to look at the wrong system, and would hide
		// that the credential may well have been stored.
		writeError(w, http.StatusBadGateway, "provider_unavailable", err.Error())
	case errors.Is(err, domain.ErrSuperseded):
		writeError(w, http.StatusConflict, "superseded", err.Error())
	default:
		// The message is deliberately generic: an internal failure's text can
		// carry paths, SQL or driver detail, none of which belongs in a
		// response. The detail goes to the log instead.
		logger.Error("unhandled service error", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
	}
}

// decodeJSON reads a JSON body, capped and strict.
//
// DisallowUnknownFields matters for a PATCH: silently ignoring a misspelled
// field would report success for a change that never happened.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(v); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return fmt.Errorf("request body exceeds %d bytes", maxBodyBytes)
		}
		return fmt.Errorf("invalid JSON body: %w", err)
	}

	// Exactly one document, so a body with trailing content is rejected rather
	// than half-applied.
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return errors.New("request body must contain a single JSON object")
	}
	return nil
}
