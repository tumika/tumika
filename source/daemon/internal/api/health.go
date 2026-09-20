package api

import "net/http"

func (h *handlers) health(w http.ResponseWriter, r *http.Request) {
	// Snapshot never fails: a health check that cannot report tells the caller
	// nothing except that something, somewhere, is wrong. Component failures are
	// in the body, and the status code stays 200 so a transport error and an
	// unhealthy daemon remain distinguishable.
	writeJSON(w, http.StatusOK, h.deps.Health.Snapshot(r.Context()))
}

func (h *handlers) version(w http.ResponseWriter, r *http.Request) {
	// One call: the build identity, the channel and the update state are
	// assembled by the service, which also decides what to leave out when a
	// part of it cannot be read.
	writeJSON(w, http.StatusOK, h.deps.Health.Version(r.Context()))
}
