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

// secretPatterns match secret material by shape, wherever it appears in a
// string — a log message, an attribute value, or a captured PTY transcript.
//
// The Anthropic prefixes cover both families we handle: sk-ant-oat01-… is the
// subscription OAuth token minted by `claude setup-token`, sk-ant-api03-… is an
// API key. The bearer pattern covers tumika's own API token appearing in a
// logged Authorization header.
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`sk-ant-[A-Za-z0-9_\-]{8,}`),
	regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._\-+/=]{8,}`),
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
