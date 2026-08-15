---
status: accepted
date: 2026-08-14
---

# Credentials are envelope-encrypted in SQLite, with key custody delegated to the platform

tumika stores long-lived, high-value secrets: a Claude subscription OAuth token valid for
roughly a year, and API keys. It also has a deliberately simple persistence story — one SQLite
file is the whole state of the system, which is what makes backup, inspection and support
tractable.

Those two facts pull in opposite directions. Putting plaintext secrets in that file makes the
file itself the secret, and it will be copied: to a backup, to a support bundle, onto a laptop
for debugging. Putting the secrets somewhere else breaks the single-file property.

We decided on **envelope encryption**: the ciphertext stays in SQLite, and only the *key*
leaves, into whatever custody the platform provides.

## Decisions

- **Ciphertext in the database.** `provider_credentials` holds `ciphertext`, `nonce`, `cipher`
  and `key_ref`. The cipher is **AES-256-GCM**. The single-file property survives: the database
  is still the complete state, it is simply not readable on its own.
- **Key custody is per-platform**, chosen at startup and reported in `/v1/health`:

  | Platform | Backend | Mechanism |
  |---|---|---|
  | macOS | Keychain | `zalando/go-keyring`; requires a LaunchAgent (not a daemon) for access |
  | Linux (systemd) | `systemd-creds` | `LoadCredentialEncrypted=` in the unit; host-bound |
  | container / fallback | file | `0600` key file in the data root |

- **`TUMIKA_MASTER_KEY` overrides everything**, for containers and for operators who bring
  their own key management.
- **`key_ref` records which backend sealed each row**, so re-keying after a platform change is
  a query rather than an archaeology exercise.
- **GCM's additional authenticated data is `provider_id|kind`.** Ciphertext is bound to the row
  it belongs to.

## Considered alternatives

- **Plaintext secrets in SQLite with `0600` file permissions.** Rejected: file permissions do
  not travel with the file, and this file is explicitly meant to be backed up.
- **Store secrets *only* in the OS keystore, with no database row.** Rejected: it breaks the
  single-file state model, has no equivalent on a plain container, and makes the credential's
  metadata (status, expiry, last verification) live apart from the credential.
- **A passphrase prompted at startup.** Rejected outright: tumika is a daemon that must survive
  an unattended reboot, which is the whole point of it supervising itself.
- **age / `sops` with a keyfile.** Rejected as a dependency that buys nothing over AES-GCM from
  the standard library for a single-writer, single-host store.

## Consequences

- **Restoring the database to a different host loses the credentials, on Linux.**
  `systemd-creds` encryption is bound to the host (TPM and/or `/var/lib/systemd/credential.secret`).
  A restored database is intact and every credential row is undecryptable. **Recovery is
  re-submitting the credential**, which is a `PUT` for an API key and a fresh login for the
  Claude Code token — deliberately not a silent failure: `/v1/health` reports
  `reauth_required`. This is a real, accepted cost, chosen because host-binding is exactly what
  makes a stolen backup worthless.
- On macOS the daemon must run as a **LaunchAgent**, not a system daemon, because Keychain
  access requires a user session. This constrains the supervision design (ADR-0003).
- The container path is the weakest: a `0600` file next to the database in the same volume. It
  is honest about being a fallback, and `TUMIKA_MASTER_KEY` is the intended answer for anyone
  running containers seriously.
- Rotating the master key requires opening and resealing every row. `key_ref` makes the set of
  affected rows a query; the operation itself is not built yet.
- Ciphertext cannot be transplanted between credential rows: the AAD binding turns that into an
  authentication failure rather than a wrong-identity success.
