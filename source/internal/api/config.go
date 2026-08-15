package api

import (
	"encoding/json"
	"net/http"

	"github.com/tumika/tumika/source/internal/domain"
)

// configListResponse wraps the settings so the response is a JSON object rather
// than a bare array — an object can grow a field later without breaking clients.
type configListResponse struct {
	Settings []domain.SettingView `json:"settings"`
}

func (h *handlers) listConfig(w http.ResponseWriter, r *http.Request) {
	views, err := h.deps.Config.List(r.Context())
	if err != nil {
		writeServiceError(w, h.deps.Logger, err)
		return
	}
	writeJSON(w, http.StatusOK, configListResponse{Settings: views})
}

// patchConfigRequest is a sparse map of key to new value. Keeping values as
// RawMessage lets the service decide what each key's type is, which is where
// that knowledge belongs.
type patchConfigRequest struct {
	Settings map[string]json.RawMessage `json:"settings"`
}

func (h *handlers) patchConfig(w http.ResponseWriter, r *http.Request) {
	var req patchConfigRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	views, err := h.deps.Config.Set(r.Context(), req.Settings)
	if err != nil {
		writeServiceError(w, h.deps.Logger, err)
		return
	}
	writeJSON(w, http.StatusOK, configListResponse{Settings: views})
}

func (h *handlers) resetConfig(w http.ResponseWriter, r *http.Request) {
	if err := h.deps.Config.Reset(r.Context(), r.PathValue("key")); err != nil {
		writeServiceError(w, h.deps.Logger, err)
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}
