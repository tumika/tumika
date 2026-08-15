# A credential secret never crosses the API boundary, never reaches a log, and never rests unsealed

A secret — an OAuth token, an API key, the auth code pasted during login — exists in plaintext
in exactly three places, and nowhere else:

1. in memory, inside `domain.Credential.Secret`, between capture and sealing;
2. in the environment of a spawned `claude` process (`CLAUDE_CODE_OAUTH_TOKEN`);
3. as AES-256-GCM ciphertext in `provider_credentials`, which is not plaintext at all.

Everywhere else it is `domain.CredentialMeta` — the non-secret half: hint, account email,
status, issued/expires timestamps. `Credential` carries the secret and is **not** JSON-tagged
for the API; `CredentialMeta` is what handlers encode. That split is the mechanism, and it only
works if nobody adds a `json:"secret"` tag "for debugging".

Three obligations follow.

**The API returns `CredentialMeta`, never `Credential`.** There is no endpoint that reads a
secret back out. A secret is write-only from the client's perspective: `PUT …/credential`
accepts one and returns metadata. A test asserts no response body ever contains `sk-ant-`.

**The slog redaction handler is not optional and is not a substitute.** `platform/logging`
installs a handler that scrubs `sk-ant-…` patterns from messages and attribute values, and
redacts by key name (`secret`, `token`, `api_key`, `credential`, `password`, `authorization`).
It is a **backstop against mistakes**, not a licence to log secrets and rely on it — a token
format tumika does not recognise passes straight through. Never pass a secret to a log call in
the first place; the handler exists for the times someone logs a whole struct.

The PTY transcript is the sharp edge here: it is raw terminal output that *contains the token
by construction*. It is redacted **at capture time**, before it reaches `login_sessions.transcript`,
and again by the log handler. Not one or the other.

**A credential in a transcript is not contiguous, and pattern-matching one is not enough.** Ink
wraps the token at the terminal width and emits a cursor move in the middle of it, so the bytes
read `sk-ant-oat01-<65 chars>ESC[1B <29 chars>`. A regex anchored on the prefix matches up to
the escape and stops. `logging.Redact` therefore *walks* the token across the wrap — continuing
only when the separator shows evidence of terminal wrapping (an escape sequence or a line
break) **and** the following run is long enough to be a continuation rather than the next word.
Both conditions are load-bearing: escapes alone swallow the prose after the token, and length
alone swallows ordinary words after a space. Anything new that redacts credential material out
of terminal output goes through `Redact` rather than growing its own pattern.

**Sealing is bound to its row.** `Sealer.Seal(plaintext, aad)` takes the AAD
`provider_id|kind`, so ciphertext cannot be transplanted from one credential row to another —
opening it against a different row fails authentication rather than yielding a token under the
wrong identity. Every `Seal` and every `Open` passes that AAD. A convenience wrapper that
defaults the AAD to `nil` defeats the whole scheme, so there is not one.

## Applies to

| | |
|---|---|
| `source/internal/domain` | `Credential` (secret, never serialised) vs `CredentialMeta` (safe, JSON-tagged) |
| `source/internal/api/**` | encodes `CredentialMeta` only; the `sk-ant-` response assertion lives here |
| `source/internal/platform/logging` | the redaction `slog.Handler` — the backstop |
| `source/internal/platform/secrets` | `Seal`/`Open`, AAD binding, key custody per backend |
| `source/internal/platform/provider/claudecode/` | PTY transcript redaction at capture time |
| `source/internal/service/` (`ProviderService`, `LoginService`) | the only layers that hold plaintext |

## Example

**WRONG**:

```go
type credentialResponse struct {
    Secret string `json:"secret"`   // no endpoint may return this
    Status string `json:"status"`
}

log.Info("storing credential", "cred", cred)          // %+v prints Secret
log.Debug("pty output", "chunk", string(buf[:n]))     // the token is in that chunk
sealed, _ := sealer.Seal([]byte(cred.Secret), nil)    // unbound ciphertext
```

**RIGHT**:

```go
meta := domain.CredentialMeta{
    Hint:   hint(cred.Secret),   // e.g. last 4 chars, never the value
    Status: "active",
}
log.InfoContext(ctx, "credential stored", "provider", id, "kind", cred.Kind, "hint", meta.Hint)

aad := []byte(providerID + "|" + cred.Kind)
sealed, err := sealer.Seal([]byte(cred.Secret), aad)
```

## Why

The threat is not an attacker reading the logs — it is the ordinary lifecycle of a log line.
tumika runs as a daemon, so its output goes to the journal or a log file, gets rotated, gets
copied into a bug report, gets pasted into an issue. A subscription OAuth token is valid for
roughly a year and cannot be scoped or revoked selectively. One `%+v` of a struct is enough,
and it will be discovered long after the token has been in a dozen places.

The AAD binding guards a quieter failure: without it, a `provider_credentials` row's ciphertext
copied into another row would decrypt cleanly, and tumika would authenticate to one provider
with another's credential — a bug that looks like a mysterious 401 and is nearly impossible to
diagnose from the outside.
