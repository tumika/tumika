package api

import (
	"net/http"

	"github.com/tumika/tumika/source/internal/domain"
)

type providerListResponse struct {
	Providers []domain.ProviderView `json:"providers"`
}

func (h *handlers) listProviders(w http.ResponseWriter, r *http.Request) {
	views, err := h.deps.Providers.List(r.Context())
	if err != nil {
		writeServiceError(w, h.deps.Logger, err)
		return
	}
	writeJSON(w, http.StatusOK, providerListResponse{Providers: views})
}

func (h *handlers) getProvider(w http.ResponseWriter, r *http.Request) {
	view, err := h.deps.Providers.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		writeServiceError(w, h.deps.Logger, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (h *handlers) providerPreflight(w http.ResponseWriter, r *http.Request) {
	pf, err := h.deps.Providers.Preflight(r.Context(), r.PathValue("id"))
	if err != nil {
		writeServiceError(w, h.deps.Logger, err)
		return
	}
	writeJSON(w, http.StatusOK, pf)
}

func (h *handlers) selectProvider(w http.ResponseWriter, r *http.Request) {
	if err := h.deps.Providers.Select(r.Context(), r.PathValue("id")); err != nil {
		writeServiceError(w, h.deps.Logger, err)
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}

// putCredentialRequest is the non-interactive submission: the caller already
// holds the secret.
//
// The secret arrives here and goes straight to the service. It is never logged,
// never echoed back, and the response carries only CredentialMeta.
type putCredentialRequest struct {
	Method string `json:"method"`
	Secret string `json:"secret"`
}

func (h *handlers) putCredential(w http.ResponseWriter, r *http.Request) {
	var req putCredentialRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	meta, err := h.deps.Providers.SubmitSecret(
		r.Context(), r.PathValue("id"), domain.AuthMethod(req.Method), req.Secret)
	if err != nil {
		writeServiceError(w, h.deps.Logger, err)
		return
	}
	writeJSON(w, http.StatusOK, meta)
}

func (h *handlers) verifyCredential(w http.ResponseWriter, r *http.Request) {
	meta, err := h.deps.Providers.VerifyCredential(r.Context(), r.PathValue("id"))
	if err != nil {
		writeServiceError(w, h.deps.Logger, err)
		return
	}
	writeJSON(w, http.StatusOK, meta)
}

func (h *handlers) deleteCredential(w http.ResponseWriter, r *http.Request) {
	if err := h.deps.Providers.DeleteCredential(r.Context(), r.PathValue("id")); err != nil {
		writeServiceError(w, h.deps.Logger, err)
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}
