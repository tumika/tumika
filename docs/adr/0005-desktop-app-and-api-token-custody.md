---
status: accepted
date: 2026-09-19
---

# A desktop app at source/desktop shares the API token through Keychain custody, and does all daemon HTTP Rust-side

tumika needs a permanent macOS surface: a tray icon first, a fuller window later. It has to
talk to the daemon, and every route is behind the single bearer token, of which the daemon
stores only the SHA-256.

## Decisions

- **The desktop app lives at `source/desktop/`** (Tauri v2, Rust 1.98.1). The tray is its
  permanent surface; a fuller window is added later.

- **The API token is shared through the login Keychain.** On macOS, `tumika install` (first
  mint) and `tumika token rotate` also write the plaintext token to the login Keychain
  (service `tumika`, account `api-token`) through `platform/tokencustody` (`Custodian.Store`).
  The hash is written first. A custody failure never aborts a mint: it is returned in
  `RotateResult.CustodyErr` and warned on stderr after the token is printed. Only
  `cli.Execute` supplies the real custodian; `daemon.Options.TokenCustody` nil is a no-op, so
  tests never touch the Keychain.

- **All daemon HTTP is done Rust-side; the token never reaches webview JavaScript.** The
  Origin middleware answers 403 to any request carrying an `Origin` header when
  `AllowedOrigins` is nil, so a webview `fetch` would be refused. A Rust HTTP client sends no
  `Origin`, and passes.

- **The app reads the item by shelling out to** `security find-generic-password -s tumika -a
  api-token -w`. go-keyring stores through `security -i` on stdin, so the token is not
  visible in `ps`; the stored value carries a `go-keyring-base64:` prefix followed by base64.
  No `-T` flag is used, so the trusted reader is `/usr/bin/security`.

## Considered alternatives

- **Daemon CORS plus an Origin allowlist for `tauri://localhost`.** Rejected: it needs CORS
  headers and preflight handling that run before the bearer check, and it puts a
  full-control token in webview JavaScript.
- **A second, app-specific token.** Rejected: it needs a multi-token auth model, a migration
  and sqlc regeneration, for a credential that would still have to be shared with the app.

## Consequences

- Until the app ships, every macOS user who mints a token has a plaintext API token in the
  login Keychain that nothing reads.
- Existing installs have no Keychain item until `tumika token rotate` is run.
- Linux and Windows are out of scope; custody is a no-op there.
- Custody failure is non-fatal, so a Keychain that is locked or denied leaves the operator
  with the printed token and an app that cannot authenticate until the next rotate.
- The daemon still cannot recover a token; only the Keychain copy and the printed one exist.
