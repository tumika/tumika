package logging

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
)

// Placeholder replaces anything the redactor recognises as a secret.
const Placeholder = "[REDACTED]"

// secretPatterns match secret material whose shape survives a regex — currently
// just an Authorization header carrying tumika's own API token.
//
// Anthropic credentials are NOT matched here; see redactPrefixedTokens for why a
// pattern cannot do that job.
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._\-+/=]{8,}`),
}

// credentialPrefixes are the credential families tumika recognises by shape:
// sk-ant-oat01-… is the subscription OAuth token minted by `claude setup-token`,
// sk-ant-api03-… is an API key.
var credentialPrefixes = []string{"sk-ant-"}

const (
	// minTokenChars is how much credential-shaped material must follow a prefix
	// before it is treated as a credential rather than a mention of the prefix.
	minTokenChars = 8
	// minContinuationChars distinguishes the remainder of a wrapped token from
	// the prose that follows it. A terminal wraps at its width, so a genuine
	// continuation is a long run; the next English word rarely is.
	minContinuationChars = 8
	// maxTokenChars bounds how far a single token may be followed, so a
	// pathological stream cannot cause the whole tail to be swallowed.
	maxTokenChars = 256
)

// redactPrefixedTokens removes credential material that begins with a known
// prefix, INCLUDING material broken across terminal rows.
//
// A regex cannot do this. In a real PTY transcript the token is not contiguous —
// Ink wraps it at the terminal width and emits a cursor move in the middle of
// it, so the bytes look like:
//
//	sk-ant-oat01-<65 chars>\x1b[1B <29 chars>
//
// `sk-ant-[A-Za-z0-9_-]{8,}` matches up to the escape and stops, leaving the
// remainder of a live credential in the clear. That was not hypothetical: it was
// measured against a captured `claude setup-token` transcript, where 29
// characters survived redaction.
//
// So this walks the token instead. It continues across a separator only when
// that separator shows evidence of terminal wrapping — an escape sequence or a
// line break — and only when the following run is long enough to be a wrapped
// continuation rather than the next word. Both conditions are required: escapes
// alone would swallow the prose that follows the token, and length alone would
// swallow ordinary words after a space.
func redactPrefixedTokens(s string) string {
	var b strings.Builder
	i := 0

	for i < len(s) {
		start, prefixLen := nextPrefix(s, i)
		if start < 0 {
			b.WriteString(s[i:])
			return b.String()
		}

		end, ok := tokenEnd(s, start+prefixLen)
		if !ok {
			// A bare mention of the prefix, not a credential. Emit it and move
			// past it so we do not rescan the same position forever.
			b.WriteString(s[i : start+prefixLen])
			i = start + prefixLen
			continue
		}

		b.WriteString(s[i:start])
		b.WriteString(Placeholder)
		i = end
	}

	return b.String()
}

// nextPrefix finds the earliest credential prefix at or after i.
func nextPrefix(s string, i int) (start, length int) {
	start, length = -1, 0
	for _, p := range credentialPrefixes {
		if idx := strings.Index(s[i:], p); idx >= 0 {
			if abs := i + idx; start < 0 || abs < start {
				start, length = abs, len(p)
			}
		}
	}
	return start, length
}

// tokenEnd walks the credential body starting at i, following it across any
// terminal wrapping, and reports where it ends. ok is false when too little
// credential-shaped material follows the prefix to call it a credential.
func tokenEnd(s string, i int) (end int, ok bool) {
	end = runEnd(s, i)
	consumed := end - i
	if consumed < minTokenChars {
		return 0, false
	}

	for consumed < maxTokenChars {
		afterSep, wrapped := skipSeparator(s, end)
		if !wrapped {
			break
		}
		next := runEnd(s, afterSep)
		if next-afterSep < minContinuationChars {
			break
		}
		consumed += next - afterSep
		end = next
	}

	return end, true
}

// runEnd consumes a run of credential characters starting at i.
func runEnd(s string, i int) int {
	for i < len(s) && isTokenChar(s[i]) {
		i++
	}
	return i
}

func isTokenChar(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-'
}

// skipSeparator consumes escape sequences and whitespace starting at i. wrapped
// reports whether the separator contained an escape sequence or a line break —
// the evidence that a terminal split a token rather than a sentence continuing.
func skipSeparator(s string, i int) (end int, wrapped bool) {
	for i < len(s) {
		switch s[i] {
		case 0x1b:
			next := skipEscape(s, i)
			if next == i {
				return i, wrapped
			}
			i, wrapped = next, true
		case '\n', '\r':
			i, wrapped = i+1, true
		case ' ', '\t':
			i++
		default:
			return i, wrapped
		}
	}
	return i, wrapped
}

// skipEscape consumes one ANSI escape sequence starting at i, returning i
// unchanged if none starts there. It handles CSI (\x1b[…final), OSC
// (\x1b]…BEL or ST) and the two-byte forms — all three appear in Claude Code's
// output, which uses OSC 8 hyperlinks for the authorization URL.
func skipEscape(s string, i int) int {
	if i >= len(s) || s[i] != 0x1b || i+1 >= len(s) {
		return i
	}

	switch s[i+1] {
	case '[':
		j := i + 2
		for j < len(s) && (s[j] >= 0x30 && s[j] <= 0x3f) {
			j++
		}
		for j < len(s) && (s[j] >= 0x20 && s[j] <= 0x2f) {
			j++
		}
		if j < len(s) && s[j] >= 0x40 && s[j] <= 0x7e {
			return j + 1
		}
		return j

	case ']':
		j := i + 2
		for j < len(s) {
			if s[j] == 0x07 {
				return j + 1
			}
			if s[j] == 0x1b && j+1 < len(s) && s[j+1] == '\\' {
				return j + 2
			}
			j++
		}
		return j

	default:
		return i + 2
	}
}

// sensitiveKeys are attribute-key substrings whose value is replaced wholesale,
// regardless of shape. This catches a secret whose format we do not recognise —
// a future token prefix, another provider's key — as long as it was logged under
// an honestly-named key.
var sensitiveKeys = []string{
	"secret", "token", "apikey", "credential", "password", "passphrase",
	"authorization", "privatekey", "masterkey",
}

// Redact removes recognisable secret material from s.
//
// It is exported because redaction is required at capture time as well as at log
// time: the PTY transcript of an interactive login contains the OAuth token by
// construction, and is scrubbed before it is ever persisted. See
// .agents/rules/never-log-or-return-a-credential-secret.md — this is a backstop
// against mistakes, never a licence to pass a secret to a log call.
func Redact(s string) string {
	s = redactPrefixedTokens(s)
	for _, re := range secretPatterns {
		s = re.ReplaceAllString(s, Placeholder)
	}
	return s
}

// isSensitiveKey reports whether an attribute key names something whose value
// must never be logged.
func isSensitiveKey(key string) bool {
	norm := strings.Map(func(r rune) rune {
		switch r {
		case '_', '-', '.', ' ':
			return -1
		default:
			return r
		}
	}, strings.ToLower(key))

	for _, k := range sensitiveKeys {
		if strings.Contains(norm, k) {
			return true
		}
	}
	return false
}

// redactHandler wraps another slog.Handler and scrubs every record passing
// through it: the message, every attribute value, and the attributes captured by
// WithAttrs.
type redactHandler struct {
	inner slog.Handler
}

// NewHandler wraps h so that no record reaching it carries recognisable secret
// material.
func NewHandler(h slog.Handler) slog.Handler { return &redactHandler{inner: h} }

func (h *redactHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

func (h *redactHandler) Handle(ctx context.Context, r slog.Record) error {
	out := slog.NewRecord(r.Time, r.Level, Redact(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(redactAttr(a))
		return true
	})
	return h.inner.Handle(ctx, out)
}

func (h *redactHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	redacted := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		redacted[i] = redactAttr(a)
	}
	return &redactHandler{inner: h.inner.WithAttrs(redacted)}
}

func (h *redactHandler) WithGroup(name string) slog.Handler {
	return &redactHandler{inner: h.inner.WithGroup(name)}
}

// redactAttr scrubs a single attribute.
//
// KindAny is the case that matters most: it is how a whole struct reaches the
// log, which is the mistake this handler exists to survive. Rendering it is only
// worth the cost when the rendering actually contains something secret, so the
// original value — and its type — is preserved in the overwhelmingly common case
// where it does not.
func redactAttr(a slog.Attr) slog.Attr {
	v := a.Value.Resolve()

	if isSensitiveKey(a.Key) && v.Kind() != slog.KindGroup {
		return slog.String(a.Key, Placeholder)
	}

	switch v.Kind() {
	case slog.KindGroup:
		attrs := v.Group()
		redacted := make([]any, 0, len(attrs))
		for _, sub := range attrs {
			redacted = append(redacted, redactAttr(sub))
		}
		return slog.Group(a.Key, redacted...)

	case slog.KindString:
		return slog.String(a.Key, Redact(v.String()))

	case slog.KindAny:
		rendered := fmt.Sprintf("%+v", v.Any())
		if scrubbed := Redact(rendered); scrubbed != rendered {
			return slog.String(a.Key, scrubbed)
		}
		return slog.Attr{Key: a.Key, Value: v}

	default:
		return slog.Attr{Key: a.Key, Value: v}
	}
}
