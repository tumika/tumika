package domain

import "encoding/json"

// SettingKind is the value type a setting accepts. It exists so that validation
// is a property of the setting's definition rather than a switch somewhere in
// the service.
type SettingKind string

const (
	SettingString   SettingKind = "string"
	SettingBool     SettingKind = "bool"
	SettingDuration SettingKind = "duration"
	SettingAddress  SettingKind = "address" // host:port
)

// SettingDefinition describes a configuration key tumika understands.
//
// The set of definitions is closed: an unknown key is rejected rather than
// stored. The settings table is generic so that adding a knob needs no
// migration, but "no migration" is not the same as "anything goes" — a typo'd
// key that silently persisted would read back as absent, and the operator would
// be looking at a setting they believe they changed.
type SettingDefinition struct {
	Key         string      `json:"key"`
	Kind        SettingKind `json:"kind"`
	Description string      `json:"description"`
	// Default is the value used when nothing is stored. It is also what the API
	// reports, so a client can show what a reset would produce.
	Default json.RawMessage `json:"default"`
	// Secret marks a setting whose value must not be returned. None exist yet —
	// credentials are not settings — but the flag is here so that the first one
	// that does cannot be added without deciding.
	Secret bool `json:"secret,omitempty"`
}

// SettingView is a setting as a client sees it: the effective value, where it
// came from, and what it would fall back to.
type SettingView struct {
	Key         string          `json:"key"`
	Kind        SettingKind     `json:"kind"`
	Description string          `json:"description"`
	Value       json.RawMessage `json:"value"`
	Default     json.RawMessage `json:"default"`
	// IsSet distinguishes "explicitly set to the default" from "never set",
	// which matters when deciding whether an upgrade may change the value under
	// the operator.
	IsSet bool `json:"is_set"`
}
