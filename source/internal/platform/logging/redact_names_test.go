package logging_test

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/tumika/tumika/source/internal/platform/logging"
)

// A credential in a format tumika does not recognise: a future provider's token,
// or its own API token before it is minted in a known shape. Shape-based
// redaction cannot help here — which is exactly when the name-based backstop has
// to work.
const unknownFormat = "novel-format-secret-XYZ-9f3a2b1c8d"

type namedCredential struct {
	Kind   string
	Secret string
}

type wrapper struct {
	Provider string
	Cred     namedCredential
}

type counted struct {
	Name       string
	TokenCount int
	HasSecret  bool
}

func logged(t *testing.T, log func(*slog.Logger)) string {
	t.Helper()

	var buf bytes.Buffer
	logger, err := logging.New(logging.Options{Format: logging.FormatJSON, Output: &buf})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	log(logger)
	return buf.String()
}

func TestSensitiveNamesAreRedactedWhereverTheyAppear(t *testing.T) {
	tests := map[string]func(*slog.Logger){
		"group whose own key is sensitive": func(l *slog.Logger) {
			l.Info("x", slog.Group("token", "value", unknownFormat))
		},
		"group whose inner key is sensitive": func(l *slog.Logger) {
			l.Info("x", slog.Group("provider", "api_key", unknownFormat))
		},
		"struct field named Secret": func(l *slog.Logger) {
			l.Info("x", "payload", namedCredential{Kind: "oauth", Secret: unknownFormat})
		},
		"pointer to such a struct": func(l *slog.Logger) {
			l.Info("x", "payload", &namedCredential{Kind: "oauth", Secret: unknownFormat})
		},
		"struct nested inside another": func(l *slog.Logger) {
			l.Info("x", "payload", wrapper{Provider: "claude-code", Cred: namedCredential{Secret: unknownFormat}})
		},
		"slice of such structs": func(l *slog.Logger) {
			l.Info("x", "payload", []namedCredential{{Secret: unknownFormat}})
		},
		"map with a sensitive key": func(l *slog.Logger) {
			l.Info("x", "payload", map[string]string{"api_key": unknownFormat})
		},
		"attribute carried by With": func(l *slog.Logger) {
			l.With("payload", namedCredential{Secret: unknownFormat}).Info("x")
		},
	}

	for name, log := range tests {
		t.Run(name, func(t *testing.T) {
			if out := logged(t, log); strings.Contains(out, unknownFormat) {
				t.Errorf("credential reached the log:\n%s", out)
			}
		})
	}
}

// Over-redaction is a real cost, not free safety: a struct redacted wholesale
// because it counts tokens tells the reader nothing.
func TestNumericFieldsWithSensitiveNamesDoNotTriggerRedaction(t *testing.T) {
	out := logged(t, func(l *slog.Logger) {
		l.Info("x", "payload", counted{Name: "claude-code", TokenCount: 42, HasSecret: true})
	})

	for _, want := range []string{"claude-code", "42"} {
		if !strings.Contains(out, want) {
			t.Errorf("a struct with only numeric/bool sensitive-named fields was redacted; %q is missing:\n%s", want, out)
		}
	}
}

func TestOrdinaryValuesAreUntouched(t *testing.T) {
	out := logged(t, func(l *slog.Logger) {
		l.Info("credential stored",
			"provider", "claude-code",
			"kind", "oauth_token",
			"attempt", 3,
			"payload", map[string]string{"status": "active", "hint": "…t9U"})
	})

	for _, want := range []string{"credential stored", "claude-code", "active", `"attempt":3`} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q to survive:\n%s", want, out)
		}
	}
}

// isSensitiveKey matches substrings, so "token" also matches input_tokens and
// friends. A count is not a credential, and redacting it destroyed the most
// operationally useful telemetry a daemon driving an LLM emits — while also
// changing the attribute from a number to a string.
func TestUsageCountersSurviveRedaction(t *testing.T) {
	out := logged(t, func(l *slog.Logger) {
		l.Info("inference complete",
			"provider", "claude-code",
			"input_tokens", 1200,
			"output_tokens", 340,
			"cache_read_tokens", 8000,
			"total_tokens", 9540,
			"token_count", 42,
			"credential_age_seconds", 86400,
			"has_secret", true,
			"duration_ms", 812)
	})

	for _, want := range []string{
		`"input_tokens":1200`,
		`"output_tokens":340`,
		`"cache_read_tokens":8000`,
		`"total_tokens":9540`,
		`"token_count":42`,
		`"credential_age_seconds":86400`,
		`"has_secret":true`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %s to survive — a number cannot be a credential:\n%s", want, out)
		}
	}
}

// The exemption is by VALUE KIND, not by key spelling: a sensitively-named key
// holding a string is still redacted.
func TestSensitiveKeysHoldingStringsAreStillRedacted(t *testing.T) {
	out := logged(t, func(l *slog.Logger) {
		l.Info("x", "token", unknownFormat, "api_key", unknownFormat, "input_tokens", 5)
	})

	if strings.Contains(out, unknownFormat) {
		t.Errorf("a string under a sensitive key survived:\n%s", out)
	}
	if !strings.Contains(out, `"input_tokens":5`) {
		t.Errorf("the counter should have survived alongside:\n%s", out)
	}
}
