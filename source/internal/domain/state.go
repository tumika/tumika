package domain

import "time"

// Setting is one row of the generic key/value configuration store. Value is
// JSON, so a new configuration knob needs no migration.
type Setting struct {
	Key       string
	Value     []byte
	UpdatedAt time.Time
}

// UpdateStatus is where the self-update state machine has got to.
type UpdateStatus string

const (
	// UpdateIdle means no update is in flight.
	UpdateIdle UpdateStatus = "idle"
	// UpdatePending means the binary has been replaced and the daemon has
	// exited; the next boot must confirm it.
	UpdatePending UpdateStatus = "pending"
	// UpdateConfirmed means the new binary booted and served successfully.
	UpdateConfirmed UpdateStatus = "confirmed"
	// UpdateRolledBack means the new binary failed to boot enough times that
	// the previous one was restored.
	UpdateRolledBack UpdateStatus = "rolled_back"
)

// MaxBootAttempts is how many times a pending update may fail to boot before
// the previous binary is restored (ADR-0003).
const MaxBootAttempts = 3

// UpdateState is the single row tracking a self-update across the process
// restart that completes it. It has to survive that restart, which is the whole
// reason it is in the database rather than in memory.
type UpdateState struct {
	Status       UpdateStatus
	FromVersion  string
	ToVersion    string
	BootAttempts int
	StartedAt    *time.Time
	UpdatedAt    time.Time
}

// ShouldRollBack reports whether a pending update has failed to boot often
// enough that the previous binary must be restored.
func (u UpdateState) ShouldRollBack() bool {
	return u.Status == UpdatePending && u.BootAttempts >= MaxBootAttempts
}
