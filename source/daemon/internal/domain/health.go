package domain

import "time"

// Health is the daemon's self-report.
//
// It is deliberately a description of what is true rather than a verdict: a
// client decides what "healthy" means for its purpose. The one judgement it does
// make is Status, which exists so a human reading a terminal does not have to.
type Health struct {
	Status  string `json:"status"` // "ok" | "degraded"
	Version string `json:"version"`
	// Uptime is rounded to the second; sub-second precision here is noise.
	Uptime  string    `json:"uptime"`
	Started time.Time `json:"started"`

	Database DatabaseHealth `json:"database"`
	Auth     AuthHealth     `json:"auth"`
	Secrets  SecretsHealth  `json:"secrets"`

	// Warnings names everything that made Status "degraded", so the reason
	// travels with the verdict rather than having to be inferred.
	Warnings []string `json:"warnings,omitempty"`
}

// DatabaseHealth describes persistence.
type DatabaseHealth struct {
	SchemaVersion int64  `json:"schema_version"`
	Reachable     bool   `json:"reachable"`
	Error         string `json:"error,omitempty"`
}

// SecretsHealth describes credential key custody. It names the backend and
// nothing else — never the key, never its reference beyond the backend name,
// since a key reference includes a filesystem path.
type SecretsHealth struct {
	Backend string `json:"backend"`
}

// AuthHealth describes API authentication. It reports only whether a token
// exists — never the token, and never its hash.
type AuthHealth struct {
	TokenConfigured bool `json:"token_configured"`
}
