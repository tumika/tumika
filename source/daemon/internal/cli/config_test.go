package cli

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/tumika/tumika/source/daemon/internal/domain"
	"github.com/tumika/tumika/source/daemon/internal/service"
)

// fakeConfigRepo is an in-memory stand-in for repository.ConfigRepository, so
// runConfigXxx is exercised against the real ConfigService — its validation
// and its sentinel errors — without a database.
type fakeConfigRepo struct {
	data map[string]domain.Setting
}

func newFakeConfigRepo() *fakeConfigRepo {
	return &fakeConfigRepo{data: map[string]domain.Setting{}}
}

func (f *fakeConfigRepo) Get(_ context.Context, key string) (domain.Setting, error) {
	s, ok := f.data[key]
	if !ok {
		return domain.Setting{}, domain.ErrNotFound
	}
	return s, nil
}

func (f *fakeConfigRepo) List(_ context.Context) ([]domain.Setting, error) {
	out := make([]domain.Setting, 0, len(f.data))
	for _, s := range f.data {
		out = append(out, s)
	}
	return out, nil
}

func (f *fakeConfigRepo) Upsert(_ context.Context, s domain.Setting) error {
	f.data[s.Key] = s
	return nil
}

func (f *fakeConfigRepo) Delete(_ context.Context, key string) error {
	delete(f.data, key)
	return nil
}

// directTxer runs the callback with no bookkeeping, so a test's assertions
// see writes immediately.
type directTxer struct{}

func (directTxer) InTx(ctx context.Context, fn func(context.Context) error) error { return fn(ctx) }

func newTestConfigService() service.ConfigService {
	return service.NewConfigService(newFakeConfigRepo(), directTxer{})
}

func TestConfigListText(t *testing.T) {
	cfg := newTestConfigService()

	out, _, err := run2(t, func(cmd *cobra.Command) error {
		return runConfigList(cmd, cfg, false)
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, want := range []string{"KEY", "VALUE", "DEFAULT", "IS_SET", service.KeyUpdateAutoApply} {
		if !strings.Contains(out, want) {
			t.Errorf("list output missing %q:\n%s", want, out)
		}
	}
	// The secret setting is invisible to every public method, including List.
	if strings.Contains(out, service.KeyAPITokenHash) {
		t.Errorf("list must never mention a secret setting:\n%s", out)
	}
}

func TestConfigListJSON(t *testing.T) {
	cfg := newTestConfigService()

	out, _, err := run2(t, func(cmd *cobra.Command) error {
		return runConfigList(cmd, cfg, true)
	})
	if err != nil {
		t.Fatalf("list --json: %v", err)
	}

	var views []domain.SettingView
	if err := json.Unmarshal([]byte(out), &views); err != nil {
		t.Fatalf("list --json is not valid JSON (%v):\n%s", err, out)
	}
	if len(views) == 0 {
		t.Fatal("list --json returned no settings")
	}
	for _, v := range views {
		if v.Key == service.KeyAPITokenHash {
			t.Error("list --json must never mention a secret setting")
		}
	}
}

func TestConfigGetReportsTheDefaultWhenUnset(t *testing.T) {
	cfg := newTestConfigService()

	out, _, err := run2(t, func(cmd *cobra.Command) error {
		return runConfigGet(cmd, cfg, service.KeyUpdateChannel, false)
	})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if strings.TrimSpace(out) != "stable" {
		t.Errorf("get = %q, want the default channel", out)
	}
}

func TestConfigGetUnknownKey(t *testing.T) {
	cfg := newTestConfigService()

	_, _, err := run2(t, func(cmd *cobra.Command) error {
		return runConfigGet(cmd, cfg, "no.such.key", false)
	})
	if !errors.Is(err, service.ErrUnknownSetting) {
		t.Fatalf("err = %v, want ErrUnknownSetting", err)
	}
}

func TestConfigSetBool(t *testing.T) {
	cfg := newTestConfigService()

	out, errOut, err := run2(t, func(cmd *cobra.Command) error {
		return runConfigSet(cmd, cfg, service.KeyUpdateAutoApply, "false", false)
	})
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if !strings.Contains(out, service.KeyUpdateAutoApply+" = false") {
		t.Errorf("set output missing the stored value:\n%s", out)
	}
	if !strings.Contains(errOut, "note: a running daemon picks this up on its next read") {
		t.Errorf("set stderr missing the live-daemon note:\n%s", errOut)
	}

	view, err := cfg.Get(context.Background(), service.KeyUpdateAutoApply)
	if err != nil {
		t.Fatalf("get after set: %v", err)
	}
	if !view.IsSet {
		t.Error("is_set = false after a successful set")
	}
	if string(view.Value) != "false" {
		t.Errorf("stored value = %s, want false", view.Value)
	}
}

func TestConfigSetString(t *testing.T) {
	cfg := newTestConfigService()

	out, errOut, err := run2(t, func(cmd *cobra.Command) error {
		return runConfigSet(cmd, cfg, service.KeyProviderSelected, "claude-code", true)
	})
	if err != nil {
		t.Fatalf("set --json: %v", err)
	}

	var view domain.SettingView
	if err := json.Unmarshal([]byte(out), &view); err != nil {
		t.Fatalf("set --json stdout is not valid JSON (%v):\n%s", err, out)
	}
	if displayValue(view.Value) != "claude-code" {
		t.Errorf("set --json value = %s, want claude-code", view.Value)
	}
	if !strings.Contains(errOut, "note: a running daemon picks this up on its next read") {
		t.Errorf("set --json stderr missing the live-daemon note:\n%s", errOut)
	}
}

func TestConfigSetRejectedValueSurfacesTheServiceError(t *testing.T) {
	cfg := newTestConfigService()

	_, _, err := run2(t, func(cmd *cobra.Command) error {
		return runConfigSet(cmd, cfg, service.KeyUpdateChannel, "nightly", false)
	})
	if !errors.Is(err, service.ErrInvalidSetting) {
		t.Fatalf("err = %v, want ErrInvalidSetting", err)
	}
}

func TestConfigSetUnknownKey(t *testing.T) {
	cfg := newTestConfigService()

	_, _, err := run2(t, func(cmd *cobra.Command) error {
		return runConfigSet(cmd, cfg, "no.such.key", "x", false)
	})
	if !errors.Is(err, service.ErrUnknownSetting) {
		t.Fatalf("err = %v, want ErrUnknownSetting", err)
	}
}

func TestConfigResetReportsTheDefaultAndIsSetFalse(t *testing.T) {
	cfg := newTestConfigService()
	ctx := context.Background()

	if _, err := cfg.Set(ctx, map[string]json.RawMessage{
		service.KeyUpdateAutoApply: json.RawMessage("false"),
	}); err != nil {
		t.Fatalf("set: %v", err)
	}

	out, errOut, err := run2(t, func(cmd *cobra.Command) error {
		return runConfigReset(cmd, cfg, service.KeyUpdateAutoApply, false)
	})
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	if !strings.Contains(out, service.KeyUpdateAutoApply+" reset to default (true), no longer explicitly set.") {
		t.Errorf("reset message wording changed:\n%s", out)
	}
	if !strings.Contains(errOut, "note: a running daemon picks this up on its next read") {
		t.Errorf("reset stderr missing the live-daemon note:\n%s", errOut)
	}

	view, err := cfg.Get(ctx, service.KeyUpdateAutoApply)
	if err != nil {
		t.Fatalf("get after reset: %v", err)
	}
	if view.IsSet {
		t.Error("is_set = true after reset")
	}
}

func TestConfigResetUnknownKey(t *testing.T) {
	cfg := newTestConfigService()

	_, _, err := run2(t, func(cmd *cobra.Command) error {
		return runConfigReset(cmd, cfg, "no.such.key", false)
	})
	if !errors.Is(err, service.ErrUnknownSetting) {
		t.Fatalf("err = %v, want ErrUnknownSetting", err)
	}
}

func TestConfigCommandIsRegistered(t *testing.T) {
	root := newRootCmd()
	cmd, _, err := root.Find([]string{"config", "list"})
	if err != nil || cmd.Name() != "list" {
		t.Fatalf("`config list` is not registered: %v", err)
	}
}
