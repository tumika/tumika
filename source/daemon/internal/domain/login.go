package domain

import "time"

// LoginState is the state machine an interactive login moves through. Only
// providers with an interactive auth method ever create one.
//
// The transitions were confirmed against a real `claude setup-token` capture:
// with no browser available — which is every daemon — the CLI emits the
// authorization URL and then blocks at its paste prompt indefinitely. It does
// not self-complete and does not time out, so AwaitingCode is a state tumika
// must genuinely drive rather than wait out.
type LoginState string

const (
	LoginPending         LoginState = "pending"
	LoginLaunching       LoginState = "launching"
	LoginAwaitingBrowser LoginState = "awaiting_browser"
	LoginAwaitingCode    LoginState = "awaiting_code"
	LoginVerifying       LoginState = "verifying"
	LoginSucceeded       LoginState = "succeeded"
	LoginFailed          LoginState = "failed"
	LoginTimedOut        LoginState = "timed_out"
	LoginCanceled        LoginState = "canceled"
)

// Terminal reports whether the session has finished. A non-terminal session
// cannot survive a daemon restart — the PTY and its child process are gone — so
// every non-terminal row is failed at startup.
func (s LoginState) Terminal() bool {
	switch s {
	case LoginSucceeded, LoginFailed, LoginTimedOut, LoginCanceled:
		return true
	default:
		return false
	}
}

// LoginSession is a persisted interactive login.
type LoginSession struct {
	ID         string // uuid
	ProviderID string
	State      LoginState

	// AuthURL is the authorization URL scraped from the provider's output, for
	// the operator to open in their own browser. It is not a secret.
	AuthURL string
	// Prompt is the provider's current human-readable prompt, surfaced to the
	// client so the UI can echo what the CLI is asking for.
	Prompt string

	ErrorCode    string
	ErrorMessage string

	// CredentialID is set once a login has produced a stored credential.
	CredentialID *int64
	// ChildPID is the spawned process, recorded so a cancel can kill the whole
	// process group.
	ChildPID *int

	// Transcript is the captured terminal output, REDACTED at capture time. It
	// contains the token by construction, so it is scrubbed before it is ever
	// written here — never afterwards.
	Transcript string

	CreatedAt time.Time
	UpdatedAt time.Time
	ExpiresAt time.Time
}

// LoginEvent is a step in an interactive login, streamed to the client so a UI
// can follow along without polling. The PTY driver produces these; the login
// session endpoints forward them.
type LoginEvent struct {
	State   LoginState `json:"state"`
	AuthURL string     `json:"auth_url,omitempty"`
	Prompt  string     `json:"prompt,omitempty"`
	Message string     `json:"message,omitempty"`
	Err     string     `json:"error,omitempty"`
}
