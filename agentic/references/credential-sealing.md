# Credential sealing

Envelope encryption (ADR-0002): AES-256-GCM ciphertext stays in SQLite, and only
the **key** leaves. That is what keeps "the database is the whole state" true —
the file is still complete and backup-able, just not readable on its own.

The cipher is fixed; only key custody varies, chosen at startup and reported by
`/v1/health`:

| Precedence | Backend | Notes |
|---|---|---|
| 1 | `TUMIKA_MASTER_KEY` | explicit beats implicit, or the override would not be trustworthy |
| 2 | systemd handover | `$CREDENTIALS_DIRECTORY` — as deliberate as the override; systemd only sets it because the unit asked |
| 3 | macOS Keychain | why the Mac install is a LaunchAgent, not a daemon — Keychain needs a session |
| 4 | `0600` file | the honest fallback; a container has no keystore |

**`systemd-creds` is split across two processes, and neither half makes sense
alone.** `tumika install` runs as root and *seals* the key, because
`systemd-creds encrypt` reads the root-only host key. The daemon runs
unprivileged and never invokes `systemd-creds` at all: the unit declares
`LoadCredentialEncrypted=`, so systemd decrypts during startup — while still
privileged — and drops the plaintext into a tmpfs the service account can read.

Decrypting at runtime is the obvious design and it does not work; the daemon
cannot read the host key and silently fell back to reporting backend `file`.
Install therefore also *proves* the handover with a transient probe unit before
committing to it: a host can be able to seal a key and unable to receive one, and
committing without checking leaves a daemon that can never start. Both were found
by running the install under a real systemd (`deploy/testharness/verify.sh`), not
by reasoning about it.

**A sealed blob that cannot be opened is fatal**, exactly like the Keychain: a
host with `master.cred` has credentials sealed under that key, and minting a
fresh one in a file would start cleanly and read none of them.

Two things that are easy to get wrong and are pinned by tests:

- **Every seal draws a fresh nonce.** GCM does not degrade under nonce reuse, it
  collapses. Never derive one from a counter or the plaintext.
- **The AAD binds ciphertext to its row** (`provider_id|kind`). Without it a row
  copied between providers decrypts cleanly, and tumika authenticates to one
  provider with another's credential — which presents as a mysterious 401.

`secrets.OpenKeyStore` *selects* a backend; `NewFileKeyStore` / `NewEnvKeyStore`
*construct* one. **Tests must never reach the selector**: on a Mac it reaches for
the real login Keychain and writes a key into it, so `go test ./...` would mutate
the Keychain of whoever ran it. Unit tests use the constructors; anything that
builds a `daemon` sets `TUMIKA_MASTER_KEY` (see `useTestKeyCustody`).

The API token's Keychain copy follows the same rule from the other direction:
`daemon.Options.TokenCustody` nil means store nothing, and only `cli.Execute`
supplies `tokencustody.New()`. A test that builds a daemon or a command tree
gets the no-op without asking.

**There is no fallback off the Keychain on macOS, deliberately.** A locked
keychain or a denied prompt fails the daemon closed. Falling back to a file would
find no key, mint a fresh one, start cleanly — and be unable to open a single
existing credential, while anything re-submitted during that run got sealed under
the new key and orphaned on the next successful start.
