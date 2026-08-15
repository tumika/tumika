package api

import (
	"net/http"

	"github.com/tumika/tumika/source/internal/platform/buildinfo"
)

func (h *handlers) health(w http.ResponseWriter, r *http.Request) {
	// Snapshot never fails: a health check that cannot report tells the caller
	// nothing except that something, somewhere, is wrong. Component failures are
	// in the body, and the status code stays 200 so a transport error and an
	// unhealthy daemon remain distinguishable.
	writeJSON(w, http.StatusOK, h.deps.Health.Snapshot(r.Context()))
}

// versionResponse is build information. Update state joins it once UpdateService
// exists; there is nothing to report until then.
type versionResponse struct {
	buildinfo.Info
}

func (h *handlers) version(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, versionResponse{Info: buildinfo.Get()})
}
