// Package service is layer 2: all business logic and every transaction
// boundary. It is the only layer that may call a repository.
//
// Services depend on repository and platform *interfaces*, never on their
// implementations — which is what lets business logic be tested against
// in-memory fakes with no database and no HTTP. See
// .agents/rules/all-business-logic-lives-in-the-service-layer.md.
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sort"
	"time"

	"github.com/tumika/tumika/source/internal/domain"
	"github.com/tumika/tumika/source/internal/repository"
)

// Config keys. Referenced from code rather than spelled as literals at each use,
// so a rename is a compile error rather than a setting that silently reverts to
// its default.
const (
	KeyServerListen        = "server.listen"
	KeyUpdateCheckInterval = "update.check_interval"
	KeyUpdateAutoApply     = "update.auto_apply"
	KeyProviderSelected    = "provider.selected"
	KeyClaudeCodeAutoLogin = "provider.claude_code.auto_login_enabled"
	// gosec G101 flags this because the identifier contains "credential" and
	// the value is a string literal. It is a config key naming how often to
	// re-verify, not a credential.
	KeyCredentialCheckInterval = "credential.check_interval" // #nosec G101 -- a config key, not a credential
)

// settingDefinitions is the closed set of keys tumika understands.
//
// Adding one here is the whole cost of a new knob — the settings table is a
// generic key/value store, so no migration is involved.
var settingDefinitions = []domain.SettingDefinition{
	{
		Key:         KeyServerListen,
		Kind:        domain.SettingAddress,
		Description: "Address the HTTP API listens on. Loopback by default: the API carries a bearer token in clear text, so binding it wider needs a private network.",
		Default:     json.RawMessage(`"127.0.0.1:8737"`),
	},
	{
		Key:         KeyUpdateCheckInterval,
		Kind:        domain.SettingDuration,
		Description: "How often to check for a new tumika release.",
		Default:     json.RawMessage(`"6h"`),
	},
	{
		Key:         KeyUpdateAutoApply,
		Kind:        domain.SettingBool,
		Description: "Apply an available update automatically rather than waiting to be told.",
		Default:     json.RawMessage(`false`),
	},
	{
		Key:         KeyProviderSelected,
		Kind:        domain.SettingString,
		Description: "Provider used for inference. Empty means none has been chosen yet.",
		Default:     json.RawMessage(`""`),
	},
	{
		Key:         KeyClaudeCodeAutoLogin,
		Kind:        domain.SettingBool,
		Description: "Drive `claude setup-token` under a PTY instead of accepting a token pasted by hand. Off until exercised on real hardware.",
		Default:     json.RawMessage(`false`),
	},
	{
		Key:         KeyCredentialCheckInterval,
		Kind:        domain.SettingDuration,
		Description: "How often to re-verify stored credentials. This is what actually detects an expired subscription token, because its expiry is an estimate.",
		Default:     json.RawMessage(`"24h"`),
	},
}

// ErrUnknownSetting is returned for a key that is not in the closed set.
var ErrUnknownSetting = errors.New("unknown setting")

// ErrInvalidSetting is returned when a value does not match its key's kind.
var ErrInvalidSetting = errors.New("invalid setting value")

// ConfigService reads and writes configuration.
type ConfigService interface {
	// Definitions returns the closed set of known settings.
	Definitions() []domain.SettingDefinition
	// Get returns one setting's effective value, falling back to its default.
	Get(ctx context.Context, key string) (domain.SettingView, error)
	// List returns every known setting, ordered by key.
	List(ctx context.Context) ([]domain.SettingView, error)
	// Set validates and stores values. It is all-or-nothing: a batch containing
	// one bad key changes nothing.
	Set(ctx context.Context, values map[string]json.RawMessage) ([]domain.SettingView, error)
	// Reset removes a stored value so the key falls back to its default.
	Reset(ctx context.Context, key string) error
}

type configService struct {
	repo repository.ConfigRepository
	tx   repository.Txer
	defs map[string]domain.SettingDefinition
}

// NewConfigService builds the service. It takes the repository it owns and a
// transaction boundary — no other repository, and no HTTP.
func NewConfigService(repo repository.ConfigRepository, tx repository.Txer) ConfigService {
	defs := make(map[string]domain.SettingDefinition, len(settingDefinitions))
	for _, d := range settingDefinitions {
		defs[d.Key] = d
	}
	return &configService{repo: repo, tx: tx, defs: defs}
}

func (s *configService) Definitions() []domain.SettingDefinition {
	out := make([]domain.SettingDefinition, len(settingDefinitions))
	copy(out, settingDefinitions)
	return out
}

func (s *configService) Get(ctx context.Context, key string) (domain.SettingView, error) {
	def, ok := s.defs[key]
	if !ok {
		return domain.SettingView{}, fmt.Errorf("%w: %q", ErrUnknownSetting, key)
	}

	stored, err := s.repo.Get(ctx, key)
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return viewOf(def, nil), nil
	case err != nil:
		return domain.SettingView{}, err
	}
	return viewOf(def, stored.Value), nil
}

func (s *configService) List(ctx context.Context) ([]domain.SettingView, error) {
	stored, err := s.repo.List(ctx)
	if err != nil {
		return nil, err
	}

	byKey := make(map[string][]byte, len(stored))
	for _, st := range stored {
		byKey[st.Key] = st.Value
	}

	out := make([]domain.SettingView, 0, len(settingDefinitions))
	for _, def := range settingDefinitions {
		out = append(out, viewOf(def, byKey[def.Key]))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// Set validates every value before writing any of them, and writes them in one
// transaction.
//
// Partial application is the failure worth avoiding here: a PATCH that set the
// listen address and then rejected the update interval would leave the daemon
// listening somewhere the operator did not intend, reported as an error they
// would reasonably read as "nothing happened".
func (s *configService) Set(ctx context.Context, values map[string]json.RawMessage) ([]domain.SettingView, error) {
	if len(values) == 0 {
		return nil, fmt.Errorf("%w: no values supplied", ErrInvalidSetting)
	}

	type change struct {
		def   domain.SettingDefinition
		value json.RawMessage
	}
	changes := make([]change, 0, len(values))

	for key, raw := range values {
		def, ok := s.defs[key]
		if !ok {
			return nil, fmt.Errorf("%w: %q", ErrUnknownSetting, key)
		}
		normalised, err := validate(def, raw)
		if err != nil {
			return nil, err
		}
		changes = append(changes, change{def: def, value: normalised})
	}

	err := s.tx.InTx(ctx, func(ctx context.Context) error {
		now := time.Now()
		for _, c := range changes {
			err := s.repo.Upsert(ctx, domain.Setting{
				Key:       c.def.Key,
				Value:     c.value,
				UpdatedAt: now,
			})
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	out := make([]domain.SettingView, 0, len(changes))
	for _, c := range changes {
		out = append(out, viewOf(c.def, c.value))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func (s *configService) Reset(ctx context.Context, key string) error {
	if _, ok := s.defs[key]; !ok {
		return fmt.Errorf("%w: %q", ErrUnknownSetting, key)
	}
	return s.repo.Delete(ctx, key)
}

func viewOf(def domain.SettingDefinition, stored []byte) domain.SettingView {
	view := domain.SettingView{
		Key:         def.Key,
		Kind:        def.Kind,
		Description: def.Description,
		Value:       def.Default,
		Default:     def.Default,
	}
	if len(stored) > 0 {
		view.Value = json.RawMessage(stored)
		view.IsSet = true
	}
	return view
}

// validate checks a value against its kind and returns it in canonical form.
//
// It is deliberately strict about types: JSON makes `"true"` and `true` easy to
// confuse, and a bool setting silently holding a string would fail much later,
// somewhere that has no idea why.
func validate(def domain.SettingDefinition, raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("%w: %s has no value", ErrInvalidSetting, def.Key)
	}

	switch def.Kind {
	case domain.SettingBool:
		var b bool
		if err := json.Unmarshal(raw, &b); err != nil {
			return nil, fmt.Errorf("%w: %s expects true or false", ErrInvalidSetting, def.Key)
		}
		return mustMarshal(b), nil

	case domain.SettingString:
		var str string
		if err := json.Unmarshal(raw, &str); err != nil {
			return nil, fmt.Errorf("%w: %s expects a string", ErrInvalidSetting, def.Key)
		}
		return mustMarshal(str), nil

	case domain.SettingDuration:
		var str string
		if err := json.Unmarshal(raw, &str); err != nil {
			return nil, fmt.Errorf("%w: %s expects a duration string such as \"6h\"", ErrInvalidSetting, def.Key)
		}
		d, err := time.ParseDuration(str)
		if err != nil {
			return nil, fmt.Errorf("%w: %s: %s is not a duration", ErrInvalidSetting, def.Key, str)
		}
		if d <= 0 {
			return nil, fmt.Errorf("%w: %s must be positive", ErrInvalidSetting, def.Key)
		}
		return mustMarshal(d.String()), nil

	case domain.SettingAddress:
		var str string
		if err := json.Unmarshal(raw, &str); err != nil {
			return nil, fmt.Errorf("%w: %s expects a host:port string", ErrInvalidSetting, def.Key)
		}
		if _, _, err := net.SplitHostPort(str); err != nil {
			return nil, fmt.Errorf("%w: %s: %s is not host:port", ErrInvalidSetting, def.Key, str)
		}
		return mustMarshal(str), nil

	default:
		return nil, fmt.Errorf("%w: %s has an unhandled kind %q", ErrInvalidSetting, def.Key, def.Kind)
	}
}

// mustMarshal is safe for the types above: bool and string always marshal.
func mustMarshal(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("marshalling a validated setting value: %v", err))
	}
	return b
}

// Duration reads a duration-kind setting, applying the default when unset. The
// runners that schedule work take their intervals through this, so a bad stored
// value cannot stop the daemon starting.
func Duration(ctx context.Context, cfg ConfigService, key string) (time.Duration, error) {
	view, err := cfg.Get(ctx, key)
	if err != nil {
		return 0, err
	}
	var str string
	if err := json.Unmarshal(view.Value, &str); err != nil {
		return 0, fmt.Errorf("%w: %s is not a duration string", ErrInvalidSetting, key)
	}
	return time.ParseDuration(str)
}

// String reads a string- or address-kind setting, applying the default when unset.
func String(ctx context.Context, cfg ConfigService, key string) (string, error) {
	view, err := cfg.Get(ctx, key)
	if err != nil {
		return "", err
	}
	var str string
	if err := json.Unmarshal(view.Value, &str); err != nil {
		return "", fmt.Errorf("%w: %s is not a string", ErrInvalidSetting, key)
	}
	return str, nil
}

// Bool reads a bool-kind setting, applying the default when unset.
func Bool(ctx context.Context, cfg ConfigService, key string) (bool, error) {
	view, err := cfg.Get(ctx, key)
	if err != nil {
		return false, err
	}
	var b bool
	if err := json.Unmarshal(view.Value, &b); err != nil {
		return false, fmt.Errorf("%w: %s is not a bool", ErrInvalidSetting, key)
	}
	return b, nil
}
