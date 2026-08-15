package logging_test

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/tumika/tumika/source/internal/platform/logging"
)

// oauthToken and apiKey are shaped like the real thing but are not credentials:
// the prefixes are what the redactor matches on, the tails are filler.
const (
	oauthToken = "sk-ant-oat01-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	apiKey     = "sk-ant-api03-BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
)

func TestRedact(t *testing.T) {
	tests := []struct {
		name  string
		in    string
		gone  string // must not appear in the output
		still string // must survive, so redaction is not just blanking everything
	}{
		{
			name:  "oauth token",
			in:    "captured token " + oauthToken + " for provider claude-code",
			gone:  oauthToken,
			still: "provider claude-code",
		},
		{
			name:  "api key",
			in:    "x-api-key: " + apiKey,
			gone:  apiKey,
			still: "x-api-key",
		},
		{
			name:  "bearer header",
			in:    "Authorization: Bearer abcdef0123456789abcdef",
			gone:  "abcdef0123456789abcdef",
			still: "Authorization",
		},
		{
			// The PTY transcript of an interactive login contains the token
			// wrapped in terminal noise; the redactor must not need clean input.
			name:  "token embedded in terminal output",
			in:    "\x1b[2K\rPaste code here: " + oauthToken + "\x1b[0m\n",
			gone:  oauthToken,
			still: "Paste code here",
		},
		{
			name:  "nothing to redact is left alone",
			in:    "listening on 127.0.0.1:8737",
			gone:  "\x00", // sentinel: never present
			still: "listening on 127.0.0.1:8737",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := logging.Redact(tc.in)
			if strings.Contains(got, tc.gone) {
				t.Errorf("secret survived redaction:\n in: %q\nout: %q", tc.in, got)
			}
			if !strings.Contains(got, tc.still) {
				t.Errorf("redaction destroyed context %q:\nout: %q", tc.still, got)
			}
		})
	}
}

// credential mirrors the shape of a struct someone might log wholesale — the
// mistake the handler exists to survive.
type credential struct {
	Kind   string
	Secret string
}

func TestHandlerRedacts(t *testing.T) {
	tests := []struct {
		name string
		log  func(l *slog.Logger)
		gone string
	}{
		{
			name: "secret in the message",
			log:  func(l *slog.Logger) { l.Info("stored " + oauthToken) },
			gone: oauthToken,
		},
		{
			name: "secret in a string attribute",
			log:  func(l *slog.Logger) { l.Info("stored", "value", oauthToken) },
			gone: oauthToken,
		},
		{
			// Redacted by key name, not by shape: this is what catches a token
			// format the patterns do not know about.
			name: "unrecognised secret under a sensitive key",
			log:  func(l *slog.Logger) { l.Info("stored", "api_key", "totally-novel-format-9f3a") },
			gone: "totally-novel-format-9f3a",
		},
		{
			name: "whole struct logged as an attribute",
			log: func(l *slog.Logger) {
				l.Info("stored", "cred", credential{Kind: "oauth_token", Secret: oauthToken})
			},
			gone: oauthToken,
		},
		{
			name: "secret inside a group",
			log: func(l *slog.Logger) {
				l.Info("stored", slog.Group("provider", "id", "claude-code", "token", oauthToken))
			},
			gone: oauthToken,
		},
		{
			name: "secret in an attribute carried by With",
			log: func(l *slog.Logger) {
				l.With("token", oauthToken).Info("stored")
			},
			gone: oauthToken,
		},
		{
			name: "secret in a group carried by WithGroup",
			log: func(l *slog.Logger) {
				l.WithGroup("login").Info("stored", "secret", apiKey)
			},
			gone: apiKey,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger, err := logging.New(logging.Options{
				Level:  "debug",
				Format: logging.FormatJSON,
				Output: &buf,
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			tc.log(logger)

			if out := buf.String(); strings.Contains(out, tc.gone) {
				t.Errorf("secret reached the log output:\n%s", out)
			}
		})
	}
}

func TestHandlerPreservesNonSecrets(t *testing.T) {
	var buf bytes.Buffer
	logger, err := logging.New(logging.Options{Format: logging.FormatJSON, Output: &buf})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	logger.Info("credential stored", "provider", "claude-code", "kind", "oauth_token", "attempt", 3)

	out := buf.String()
	for _, want := range []string{"credential stored", "claude-code", "oauth_token", `"attempt":3`} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in output:\n%s", want, out)
		}
	}
}

func TestParseLevel(t *testing.T) {
	tests := map[string]struct {
		want    slog.Level
		wantErr bool
	}{
		"debug":   {want: slog.LevelDebug},
		"INFO":    {want: slog.LevelInfo},
		"":        {want: slog.LevelInfo},
		" warn ":  {want: slog.LevelWarn},
		"warning": {want: slog.LevelWarn},
		"error":   {want: slog.LevelError},
		"loud":    {wantErr: true},
	}

	for in, tc := range tests {
		t.Run(in, func(t *testing.T) {
			got, err := logging.ParseLevel(in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseLevel(%q) = %v, want error", in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseLevel(%q): %v", in, err)
			}
			if got != tc.want {
				t.Errorf("ParseLevel(%q) = %v, want %v", in, got, tc.want)
			}
		})
	}
}

func TestNewRejectsUnknownFormat(t *testing.T) {
	if _, err := logging.New(logging.Options{Format: "yaml"}); err == nil {
		t.Fatal("expected an error for an unknown log format")
	}
}
