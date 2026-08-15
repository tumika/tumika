package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/tumika/tumika/source/internal/domain"
	"github.com/tumika/tumika/source/internal/service"
)

// fakeConfigRepo is an in-memory stand-in for the repository.
//
// The whole point of the layering is that business logic can be tested without
// a database: these tests open no file, run no migration and touch no SQL. If a
// rule ever needs a real store to test, it has leaked out of this layer.
type fakeConfigRepo struct {
	data     map[string]domain.Setting
	listErr  error
	writeErr error
	writes   int
}

func newFakeRepo() *fakeConfigRepo {
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
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]domain.Setting, 0, len(f.data))
	for _, s := range f.data {
		out = append(out, s)
	}
	return out, nil
}

func (f *fakeConfigRepo) Upsert(_ context.Context, s domain.Setting) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	f.writes++
	f.data[s.Key] = s
	return nil
}

func (f *fakeConfigRepo) Delete(_ context.Context, key string) error {
	delete(f.data, key)
	return nil
}

// fakeTxer runs the callback directly and records that it was used, so a test
// can assert writes went through a transaction boundary at all.
type fakeTxer struct{ began int }

func (f *fakeTxer) InTx(ctx context.Context, fn func(context.Context) error) error {
	f.began++
	return fn(ctx)
}

func newService(t *testing.T) (service.ConfigService, *fakeConfigRepo, *fakeTxer) {
	t.Helper()
	repo, tx := newFakeRepo(), &fakeTxer{}
	return service.NewConfigService(repo, tx), repo, tx
}

// A secret setting must be invisible to the config API — not merely
// value-redacted. Reporting it as unknown avoids confirming it exists at all,
// and the config API has no legitimate use for it either way.
func TestSecretSettingsAreInvisibleToTheConfigAPI(t *testing.T) {
	svc, repo, _ := newService(t)
	ctx := t.Context()

	if _, err := svc.Get(ctx, service.KeyAPITokenHash); !errors.Is(err, service.ErrUnknownSetting) {
		t.Errorf("Get(secret) = %v, want ErrUnknownSetting", err)
	}
	_, err := svc.Set(ctx, map[string]json.RawMessage{service.KeyAPITokenHash: json.RawMessage(`"x"`)})
	if !errors.Is(err, service.ErrUnknownSetting) {
		t.Errorf("Set(secret) = %v, want ErrUnknownSetting", err)
	}
	if err := svc.Reset(ctx, service.KeyAPITokenHash); !errors.Is(err, service.ErrUnknownSetting) {
		t.Errorf("Reset(secret) = %v, want ErrUnknownSetting", err)
	}
	if repo.writes != 0 {
		t.Errorf("the config API wrote %d rows to a secret setting", repo.writes)
	}

	// Even once written internally, it must not appear in a listing.
	if err := svc.WriteSecret(ctx, service.KeyAPITokenHash, "deadbeef"); err != nil {
		t.Fatalf("WriteSecret: %v", err)
	}
	views, err := svc.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, v := range views {
		if v.Key == service.KeyAPITokenHash {
			t.Error("a secret setting appeared in List")
		}
		if strings.Contains(string(v.Value), "deadbeef") {
			t.Errorf("a secret value leaked into a view: %+v", v)
		}
	}
	for _, d := range svc.Definitions() {
		if d.Key == service.KeyAPITokenHash {
			t.Error("a secret setting appeared in Definitions")
		}
	}
}

func TestSecretRoundTrip(t *testing.T) {
	svc, _, _ := newService(t)
	ctx := t.Context()

	if _, err := svc.ReadSecret(ctx, service.KeyAPITokenHash); !errors.Is(err, service.ErrNotSet) {
		t.Errorf("ReadSecret before writing = %v, want ErrNotSet", err)
	}
	if err := svc.WriteSecret(ctx, service.KeyAPITokenHash, "abc123"); err != nil {
		t.Fatalf("WriteSecret: %v", err)
	}
	got, err := svc.ReadSecret(ctx, service.KeyAPITokenHash)
	if err != nil {
		t.Fatalf("ReadSecret: %v", err)
	}
	if got != "abc123" {
		t.Errorf("ReadSecret = %q", got)
	}

	// The secret door only opens for secret keys.
	if _, err := svc.ReadSecret(ctx, service.KeyServerListen); !errors.Is(err, service.ErrUnknownSetting) {
		t.Errorf("ReadSecret(public key) = %v, want ErrUnknownSetting", err)
	}
	if err := svc.WriteSecret(ctx, service.KeyServerListen, "x"); !errors.Is(err, service.ErrUnknownSetting) {
		t.Errorf("WriteSecret(public key) = %v, want ErrUnknownSetting", err)
	}
}

func TestGetFallsBackToTheDefault(t *testing.T) {
	svc, _, _ := newService(t)

	view, err := svc.Get(t.Context(), service.KeyServerListen)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(view.Value) != `"127.0.0.1:8737"` {
		t.Errorf("Value = %s, want the default", view.Value)
	}
	// "never set" and "set to the default" are different facts: only the former
	// may be changed by an upgrade without surprising the operator.
	if view.IsSet {
		t.Error("IsSet must be false when nothing is stored")
	}
}

func TestUnknownKeysAreRejected(t *testing.T) {
	svc, repo, _ := newService(t)
	ctx := t.Context()

	if _, err := svc.Get(ctx, "server.lister"); !errors.Is(err, service.ErrUnknownSetting) {
		t.Errorf("Get(typo) = %v, want ErrUnknownSetting", err)
	}
	if err := svc.Reset(ctx, "nope"); !errors.Is(err, service.ErrUnknownSetting) {
		t.Errorf("Reset(unknown) = %v, want ErrUnknownSetting", err)
	}

	_, err := svc.Set(ctx, map[string]json.RawMessage{"server.lister": json.RawMessage(`"127.0.0.1:1"`)})
	if !errors.Is(err, service.ErrUnknownSetting) {
		t.Errorf("Set(typo) = %v, want ErrUnknownSetting", err)
	}
	if repo.writes != 0 {
		t.Errorf("a typo'd key wrote %d rows; it must write none", repo.writes)
	}
}

func TestSetValidatesByKind(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		value   string
		wantErr bool
		stored  string // canonical form expected after a successful write
	}{
		{name: "bool accepts a bool", key: service.KeyUpdateAutoApply, value: `true`, stored: `true`},
		{name: "bool rejects a string", key: service.KeyUpdateAutoApply, value: `"true"`, wantErr: true},
		{name: "duration accepts 30m", key: service.KeyUpdateCheckInterval, value: `"30m"`, stored: `"30m0s"`},
		{name: "duration rejects nonsense", key: service.KeyUpdateCheckInterval, value: `"soon"`, wantErr: true},
		{name: "duration rejects zero", key: service.KeyUpdateCheckInterval, value: `"0s"`, wantErr: true},
		{name: "duration rejects negative", key: service.KeyUpdateCheckInterval, value: `"-1h"`, wantErr: true},
		{name: "duration rejects a number", key: service.KeyUpdateCheckInterval, value: `3600`, wantErr: true},
		{name: "address accepts host:port", key: service.KeyServerListen, value: `"0.0.0.0:9000"`, stored: `"0.0.0.0:9000"`},
		{name: "address rejects a bare host", key: service.KeyServerListen, value: `"0.0.0.0"`, wantErr: true},
		{name: "string accepts a string", key: service.KeyProviderSelected, value: `"claude-code"`, stored: `"claude-code"`},
		{name: "string rejects a bool", key: service.KeyProviderSelected, value: `false`, wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc, repo, _ := newService(t)

			views, err := svc.Set(t.Context(), map[string]json.RawMessage{tc.key: json.RawMessage(tc.value)})
			if tc.wantErr {
				if !errors.Is(err, service.ErrInvalidSetting) {
					t.Fatalf("Set(%s) = %v, want ErrInvalidSetting", tc.value, err)
				}
				if repo.writes != 0 {
					t.Errorf("a rejected value still wrote %d rows", repo.writes)
				}
				return
			}

			if err != nil {
				t.Fatalf("Set: %v", err)
			}
			if got := string(repo.data[tc.key].Value); got != tc.stored {
				t.Errorf("stored %s, want %s", got, tc.stored)
			}
			if len(views) != 1 || !views[0].IsSet {
				t.Errorf("Set returned %+v, want one view marked set", views)
			}
		})
	}
}

// A batch is all-or-nothing. Half-applying a PATCH would leave the daemon in a
// state the operator did not ask for, reported as an error they would
// reasonably read as "nothing happened".
func TestSetIsAllOrNothing(t *testing.T) {
	svc, repo, tx := newService(t)

	_, err := svc.Set(t.Context(), map[string]json.RawMessage{
		service.KeyServerListen:        json.RawMessage(`"0.0.0.0:9000"`), // valid
		service.KeyUpdateCheckInterval: json.RawMessage(`"soon"`),         // not
	})
	if !errors.Is(err, service.ErrInvalidSetting) {
		t.Fatalf("Set = %v, want ErrInvalidSetting", err)
	}
	if repo.writes != 0 {
		t.Errorf("%d writes happened; a rejected batch must write nothing", repo.writes)
	}
	if _, ok := repo.data[service.KeyServerListen]; ok {
		t.Error("the valid half of a rejected batch was applied")
	}
	if tx.began != 0 {
		t.Error("validation must happen before the transaction is opened")
	}
}

func TestSetUsesOneTransactionForTheWholeBatch(t *testing.T) {
	svc, repo, tx := newService(t)

	_, err := svc.Set(t.Context(), map[string]json.RawMessage{
		service.KeyServerListen:    json.RawMessage(`"0.0.0.0:9000"`),
		service.KeyUpdateAutoApply: json.RawMessage(`true`),
	})
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	if tx.began != 1 {
		t.Errorf("opened %d transactions for one batch, want 1", tx.began)
	}
	if repo.writes != 2 {
		t.Errorf("wrote %d settings, want 2", repo.writes)
	}
}

func TestSetRejectsAnEmptyBatch(t *testing.T) {
	svc, _, tx := newService(t)

	if _, err := svc.Set(t.Context(), nil); !errors.Is(err, service.ErrInvalidSetting) {
		t.Errorf("Set(nil) = %v, want ErrInvalidSetting", err)
	}
	if tx.began != 0 {
		t.Error("an empty batch must not open a transaction")
	}
}

func TestListReturnsEveryKnownSettingOrdered(t *testing.T) {
	svc, _, _ := newService(t)
	ctx := t.Context()

	if _, err := svc.Set(ctx, map[string]json.RawMessage{service.KeyUpdateAutoApply: json.RawMessage(`true`)}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	views, err := svc.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(views) != len(svc.Definitions()) {
		t.Fatalf("List returned %d settings, want every known one (%d)", len(views), len(svc.Definitions()))
	}
	for i := 1; i < len(views); i++ {
		if views[i-1].Key >= views[i].Key {
			t.Fatalf("List is not ordered by key: %s before %s", views[i-1].Key, views[i].Key)
		}
	}

	for _, v := range views {
		if v.Key == service.KeyUpdateAutoApply {
			if !v.IsSet || string(v.Value) != "true" {
				t.Errorf("stored value not reflected: %+v", v)
			}
			if string(v.Default) != "false" {
				t.Errorf("Default = %s, want the definition's default even when set", v.Default)
			}
		}
	}
}

func TestResetRestoresTheDefault(t *testing.T) {
	svc, _, _ := newService(t)
	ctx := t.Context()

	if _, err := svc.Set(ctx, map[string]json.RawMessage{service.KeyServerListen: json.RawMessage(`"0.0.0.0:9000"`)}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := svc.Reset(ctx, service.KeyServerListen); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	view, err := svc.Get(ctx, service.KeyServerListen)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if view.IsSet || string(view.Value) != `"127.0.0.1:8737"` {
		t.Errorf("after Reset: %+v, want the default and IsSet false", view)
	}
}

func TestRepositoryErrorsPropagate(t *testing.T) {
	svc, repo, _ := newService(t)
	boom := errors.New("database is gone")
	repo.listErr = boom

	if _, err := svc.List(t.Context()); !errors.Is(err, boom) {
		t.Errorf("List = %v, want the repository's error", err)
	}
}

// The typed accessors are how runners read their intervals; they must apply the
// default rather than failing when nothing is stored.
func TestTypedAccessors(t *testing.T) {
	svc, _, _ := newService(t)
	ctx := t.Context()

	d, err := service.Duration(ctx, svc, service.KeyCredentialCheckInterval)
	if err != nil {
		t.Fatalf("Duration: %v", err)
	}
	if d.Hours() != 24 {
		t.Errorf("Duration = %v, want 24h", d)
	}

	addr, err := service.String(ctx, svc, service.KeyServerListen)
	if err != nil {
		t.Fatalf("String: %v", err)
	}
	if addr != "127.0.0.1:8737" {
		t.Errorf("String = %q", addr)
	}

	// Loopback by default is a security property, not a preference: the API
	// carries a bearer token in clear text until TLS lands.
	auto, err := service.Bool(ctx, svc, service.KeyClaudeCodeAutoLogin)
	if err != nil {
		t.Fatalf("Bool: %v", err)
	}
	if auto {
		t.Error("PTY auto-login must default to off until it is exercised on real hardware")
	}
}
