---
about:
  - source/desktop/src-tauri/src/keychain.rs
  - source/desktop/src-tauri/src/health.rs
saw: 8a6624f
---

# The tray app reads the token through `/usr/bin/security` and calls the daemon only from Rust

- `keychain.rs` shells out to `/usr/bin/security find-generic-password -s tumika -a api-token -w`
  rather than using the Security framework. The item is written by go-keyring without `-T`, so its
  trusted reader is `security`; a framework read from another binary prompts. The stored value is
  `go-keyring-base64:` plus standard base64, and exit code 44 means the item does not exist, which
  is the state of every install that has not run `tumika token rotate` since the daemon began
  writing one.
- The read is async with `kill_on_drop(true)` and a 3s `READ_TIMEOUT`. A locked Keychain waits on
  an unlock dialog indefinitely; a blocking read on a runtime worker stopped the poll loop and left
  the tray showing a state it had stopped checking, and a `spawn_blocking` read cannot be killed, so
  it would have leaked one `security` process per poll.
- `health.rs` never sets an `Origin` header and disables redirects. The daemon's Origin middleware
  answers 403 to any request that carries one and `AllowedOrigins` is nil by default, which is why
  every daemon call is made Rust-side and a webview `fetch` would be refused; a redirect would carry
  the bearer token to another host.
