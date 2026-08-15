package logging_test

import (
	"strings"
	"testing"

	"github.com/tumika/tumika/source/internal/platform/logging"
)

// The token fragments below are synthetic, but the framing around them is copied
// from a real `claude setup-token` capture: Ink wraps the token at the terminal
// width and emits ESC[1B (cursor down) mid-token, then moves on to the next line
// of prose with further cursor moves. The earlier regex-based redactor matched up
// to the first escape and left the remainder — 29 characters of a live
// credential — in the clear.
const (
	wrappedHead = "sk-ant-oat01-" + "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	wrappedTail = "BBBBBBBBBBBBBBBBBBBBBBBBBBBBB"

	// A faithful slice of the captured transcript's token region.
	wrappedTranscript = " Your OAuth token (valid for 1 year):\x1b[K\x1b[1B\x1b[K\x1b[1B " +
		wrappedHead + "\x1b[1B " + wrappedTail +
		"\x1b[1C\x1b[2BStore\x1b[8Gthis\x1b[13Gtoken\x1b[19Gsecurely."
)

func TestRedactFollowsATokenAcrossTerminalWrapping(t *testing.T) {
	got := logging.Redact(wrappedTranscript)

	if strings.Contains(got, wrappedHead) {
		t.Error("the leading fragment of the token survived redaction")
	}
	if strings.Contains(got, wrappedTail) {
		t.Errorf("the wrapped continuation survived redaction — this is the exact bug:\n%q", got)
	}
	// The transcript must remain recognisable; redaction is not meant to eat it.
	for _, want := range []string{"Your OAuth token", "Store", "securely."} {
		if !strings.Contains(got, want) {
			t.Errorf("redaction swallowed surrounding text %q:\n%q", want, got)
		}
	}
}

// Following a token across wrapping must not turn into following it across
// ordinary prose. A space is not evidence of terminal wrapping.
func TestRedactDoesNotSwallowProseAfterAToken(t *testing.T) {
	got := logging.Redact("stored sk-ant-oat01-AAAAAAAAAAAAAAAA submitted successfully")

	if strings.Contains(got, "sk-ant-oat01-AAAA") {
		t.Error("token survived redaction")
	}
	for _, want := range []string{"stored", "submitted", "successfully"} {
		if !strings.Contains(got, want) {
			t.Errorf("redaction swallowed the word %q:\n%q", want, got)
		}
	}
}

// The authorization URL is not a secret, and step 14's parser reads it out of an
// OSC 8 hyperlink. Redacting it would break login rather than protect anything.
func TestRedactLeavesTheAuthorizationURLIntact(t *testing.T) {
	const url = "https://claude.com/cai/oauth/authorize?code=true&client_id=9d1c250a-e61b-44d9-88ed-5944d1962f5e" +
		"&response_type=code&scope=user%3Ainference&code_challenge_method=S256"
	transcript := "\x1b]8;id=se46jx;" + url + "\x1b\\Browser didn't open?\x1b]8;;\x1b\\"

	if got := logging.Redact(transcript); !strings.Contains(got, url) {
		t.Errorf("the authorization URL must survive redaction:\n%q", got)
	}
}

// A bare mention of the prefix — in a log line, a doc string, an error message —
// is not a credential, and must neither be redacted nor send the scanner into a
// loop.
func TestRedactIgnoresABarePrefixMention(t *testing.T) {
	const msg = "expected a secret beginning with sk-ant- but the field was empty"

	if got := logging.Redact(msg); got != msg {
		t.Errorf("Redact(%q) = %q, want it unchanged", msg, got)
	}
}

// A token split so that only a few characters land on the second row leaves that
// short remainder behind; that is a deliberate bound, not an oversight, and the
// fragment is far too short to be usable.
func TestRedactStillRemovesTheBulkOfAnAwkwardlySplitToken(t *testing.T) {
	got := logging.Redact("sk-ant-oat01-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA\x1b[1B xyz")

	if strings.Contains(got, "AAAAAAAA") {
		t.Errorf("the substantive part of the token survived:\n%q", got)
	}
}
