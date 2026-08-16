package api

import (
	"net/http"

	"github.com/tumika/tumika/source/internal/domain"
	"github.com/tumika/tumika/source/internal/platform/buildinfo"
)

func (h *handlers) health(w http.ResponseWriter, r *http.Request) {
	// Snapshot never fails: a health check that cannot report tells the caller
	// nothing except that something, somewhere, is wrong. Component failures are
	// in the body, and the status code stays 200 so a transport error and an
	// unhealthy daemon remain distinguishable.
	writeJSON(w, http.StatusOK, h.deps.Health.Snapshot(r.Context()))
}

// versionResponse is build information plus what the self-updater is doing.
//
// One response rather than two endpoints: "which version am I running" and "is
// an update half-applied" are the same question when a daemon has just
// restarted itself, and answering them separately invites reading one without
// the other.
type versionResponse struct {
	buildinfo.Info
	Update *domain.UpdateState `json:"update,omitempty"`
}

func (h *handlers) version(w http.ResponseWriter, r *http.Request) {
	resp := versionResponse{Info: buildinfo.Get()}

	// Omitted rather than fatal when it cannot be read: `version` is the one
	// endpoint that must answer on a broken install, and it is what the updater
	// execs to pre-flight a staged binary.
	if h.deps.Updates != nil {
		if state, err := h.deps.Updates.State(r.Context()); err == nil {
			resp.Update = &state
		} else {
			h.deps.Logger.WarnContext(r.Context(), "reading update state failed", "err", err)
		}
	}

	writeJSON(w, http.StatusOK, resp)
}
