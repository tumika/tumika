package api

import (
	"net/http"

	"github.com/tumika/tumika/source/internal/platform/buildinfo"
)

// currentVersion is the running build, for the check response.
func currentVersion() string { return buildinfo.Version() }

// updateUnsupported is the answer when no UpdateService is wired in.
//
// A container is the case: the image is the unit of deployment, and a container
// that rewrites its own binary no longer matches its tag (ADR-0003). Saying so
// is better than a 404, which reads as "wrong URL" rather than "not how you
// update this deployment".
func (h *handlers) updateUnsupported(w http.ResponseWriter) bool {
	if h.deps.Updates != nil {
		return false
	}
	writeError(w, http.StatusBadRequest, "update_unsupported",
		"self-update is disabled for this deployment; update the image or package instead")
	return true
}

func (h *handlers) updateState(w http.ResponseWriter, r *http.Request) {
	if h.updateUnsupported(w) {
		return
	}

	state, err := h.deps.Updates.State(r.Context())
	if err != nil {
		writeServiceError(w, h.deps.Logger, err)
		return
	}
	writeJSON(w, http.StatusOK, state)
}

// updateCheckResponse reports what is published, and whether it is newer.
//
// `newer` is computed by the service rather than left to the client: the
// comparison is semver, not string ordering, and a client doing it itself would
// eventually get 0.10.0 < 0.9.0 wrong.
type updateCheckResponse struct {
	Current   string `json:"current"`
	Available string `json:"available"`
	Newer     bool   `json:"newer"`
}

func (h *handlers) updateCheck(w http.ResponseWriter, r *http.Request) {
	if h.updateUnsupported(w) {
		return
	}

	available, newer, err := h.deps.Updates.Check(r.Context())
	if err != nil {
		writeServiceError(w, h.deps.Logger, err)
		return
	}
	writeJSON(w, http.StatusOK, updateCheckResponse{
		Current:   currentVersion(),
		Available: available,
		Newer:     newer,
	})
}

// updateApplyRequest optionally pins the version to install.
type updateApplyRequest struct {
	Version string `json:"version,omitempty"`
}

// updateApply installs an update and reports what will happen next.
//
// It deliberately does NOT exit the process. The response has to reach the
// caller first — an operator who asked for an update and got a dropped
// connection cannot tell "it worked and restarted" from "it crashed". The
// daemon exits after the response is written, on its own terms.
func (h *handlers) updateApply(w http.ResponseWriter, r *http.Request) {
	if h.updateUnsupported(w) {
		return
	}

	var req updateApplyRequest
	if r.ContentLength != 0 {
		if err := decodeJSON(w, r, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
	}

	version := req.Version
	if version == "" {
		available, newer, err := h.deps.Updates.Check(r.Context())
		if err != nil {
			writeServiceError(w, h.deps.Logger, err)
			return
		}
		if !newer {
			writeError(w, http.StatusConflict, "up_to_date",
				"the newest published release is "+available+", which is not newer than the running version")
			return
		}
		version = available
	}

	if err := h.deps.Updates.Apply(r.Context(), version); err != nil {
		writeServiceError(w, h.deps.Logger, err)
		return
	}

	state, err := h.deps.Updates.State(r.Context())
	if err != nil {
		writeServiceError(w, h.deps.Logger, err)
		return
	}
	writeJSON(w, http.StatusOK, state)

	// Signalled AFTER the response is written. The daemon decides when to act
	// on it; nothing here calls os.Exit.
	if h.deps.UpdateApplied != nil {
		h.deps.UpdateApplied()
	}
}
