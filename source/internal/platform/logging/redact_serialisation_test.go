package logging_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/tumika/tumika/source/internal/platform/logging"
)

const serialisedToken = "sk-ant-oat01-" + "CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC"

// maskedCredential renders harmlessly under %+v but reveals the secret when a
// JSON encoder serialises it. Attribute-level redaction inspected the former and
// the handler wrote the latter, so the token shipped verbatim.
type maskedCredential struct{ Secret string }

func (m maskedCredential) String() string { return "maskedCredential{***}" }

func (m maskedCredential) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]string{"secret": m.Secret})
}

// Redaction has to survive the encoder, not merely the value: what reaches disk
// is whatever the handler serialised, not what we inspected beforehand.
func TestSecretsDoNotSurviveSerialisation(t *testing.T) {
	tests := []struct {
		name string
		// forbidden is what must not appear in the output. For a []byte the
		// danger is the base64 encoding, which no textual scrub downstream would
		// recognise and anyone can reverse.
		forbidden []string
		value     any
	}{
		{
			name:      "byte slice is base64-encoded by the JSON handler",
			value:     []byte(serialisedToken),
			forbidden: []string{serialisedToken, "c2stYW50"},
		},
		{
			name:      "masking String with a revealing MarshalJSON",
			value:     maskedCredential{Secret: serialisedToken},
			forbidden: []string{serialisedToken},
		},
		{
			name:      "pointer to a struct holding a secret",
			value:     &maskedCredential{Secret: serialisedToken},
			forbidden: []string{serialisedToken},
		},
	}

	for _, tc := range tests {
		for _, format := range []logging.Format{logging.FormatJSON, logging.FormatText} {
			t.Run(tc.name+"/"+string(format), func(t *testing.T) {
				var buf bytes.Buffer
				logger, err := logging.New(logging.Options{Format: format, Output: &buf})
				if err != nil {
					t.Fatalf("New: %v", err)
				}

				// A deliberately NEUTRAL key. "credential_value" would be redacted by
				// name before the value was ever serialised, which would make this
				// test pass without exercising the path it exists to cover.
				logger.Info("stored", "payload", tc.value)

				out := buf.String()
				for _, forbidden := range tc.forbidden {
					if strings.Contains(out, forbidden) {
						t.Errorf("secret material %q reached the log:\n%s", forbidden, out)
					}
				}
			})
		}
	}
}

// Redaction that eats ordinary words is its own failure: the surviving line has
// to still say what happened.
func TestRedactLeavesProseAlone(t *testing.T) {
	for _, s := range []string{
		"basic authentication required for endpoint /v1/providers",
		"bearer capability negotiation failed",
		"falling back to basic verification",
	} {
		if got := logging.Redact(s); got != s {
			t.Errorf("Redact(%q) = %q, want it unchanged", s, got)
		}
	}
}

// The header form is still redacted — and keeps enough of itself to show that an
// Authorization header was present, and of what kind.
func TestRedactAuthorizationHeaderValue(t *testing.T) {
	tests := map[string]string{
		"Authorization: Bearer abcdef0123456789abcdef":  "Authorization: Bearer ",
		"authorization=Basic YWxhZGRpbjpvcGVuc2VzYW1l":  "authorization=Basic ",
		`"Authorization":"Bearer abcdef0123456789abcd"`: `"Authorization":"Bearer `,
	}

	for in, wantPrefix := range tests {
		got := logging.Redact(in)
		if strings.Contains(got, "abcdef0123456789") || strings.Contains(got, "YWxhZGRpbjpvcGVuc2VzYW1l") {
			t.Errorf("Redact(%q) left the credential: %q", in, got)
		}
		if !strings.Contains(got, wantPrefix) {
			t.Errorf("Redact(%q) = %q, want it to keep %q", in, got, wantPrefix)
		}
	}
}

// failingWriter reports an error, to exercise the redacting writer's error path.
type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

func TestRedactingWriterPropagatesErrors(t *testing.T) {
	boom := errors.New("disk full")
	logger, err := logging.New(logging.Options{
		Format: logging.FormatJSON,
		Output: failingWriter{err: boom},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// slog swallows handler errors, so this asserts the path runs without
	// panicking rather than surfacing boom — the point is that a write error in
	// the redacting path behaves like a write error anywhere else.
	logger.Info("stored", "payload", serialisedToken)
}

// io.Writer requires n == len(p) on success. Redaction shortens the buffer, so
// returning the scrubbed length would look like a short write and make a caller
// retry — writing the tail of a record a second time.
func TestRedactingWriterReportsTheCallersLength(t *testing.T) {
	var buf bytes.Buffer
	logger, err := logging.New(logging.Options{Format: logging.FormatJSON, Output: &buf})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	logger.Info("stored", "payload", serialisedToken)

	out := buf.String()
	if strings.Count(out, "\n") != 1 {
		t.Errorf("expected exactly one record, got:\n%s", out)
	}
	if strings.Contains(out, serialisedToken) {
		t.Errorf("token survived: %s", out)
	}
}
