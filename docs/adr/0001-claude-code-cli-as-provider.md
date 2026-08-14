---
status: accepted
date: 2026-08-14
---

# The primary provider is the vendored Claude Code CLI, driven with a subscription OAuth token

tumika is a personal assistant that runs workflows on a schedule — inbox triage every three
hours, and more later. That cadence, multiplied by the size of a curated payload, makes
Anthropic API billing the dominant cost of running it, and a cost that scales with how useful
the assistant is. Meanwhile the operator, in practice, already pays for a Claude subscription.

We decided to make the **vendored `claude` CLI a first-class provider**, authenticated with a
subscription OAuth token, and to treat API-key providers as an equal-status alternative rather
than the default.

## Decisions

- **tumika vendors its own copy of Claude Code** rather than using whatever is on `PATH`. It
  downloads a **pinned version** (currently `2.1.232`) directly from Anthropic's release
  bucket, verifies the GPG signature on `manifest.json` against fingerprint
  `31DDDE24DDFAB679F42D7BD2BAA929FF1A7ECACE` and then the per-platform SHA, and executes it by
  absolute path. No `curl | bash`, no npm, no apt, no launcher symlink.
- **The auto-updater is disabled** (`DISABLE_AUTOUPDATER=1`). The pin is the contract: the
  login flow parses a specific version's terminal output, so a silent self-update is a silent
  break.
- **The credential is an OAuth token minted by `claude setup-token`**, which prints a ~1-year
  token to the terminal and stores it nowhere. tumika captures it and owns it, re-injecting it
  as `CLAUDE_CODE_OAUTH_TOKEN`. `/login` is the wrong command — it authenticates a session, not
  a portable token.
- **A second, non-interactive provider (`anthropic-api`) ships in the same branch.** It is
  roughly a hundred lines and is the only real evidence that the provider abstraction is not
  silently CLI-shaped.
- **The provider contract is "rendered prompt in → validated JSON out"**, not an agent with
  tools. tumika owns the data plane: it fetches and filters, then hands the model a curated
  payload. This is why `Inferencer` is deferred rather than guessed at — we will define it once
  a real workflow needs it.

## Considered alternatives

- **The Anthropic API directly.** Rejected on cost, which is the product's central constraint —
  not on capability. It remains fully supported via `anthropic-api` for anyone who prefers it,
  which is why it ships now rather than later.
- **The Claude Agent SDK.** Rejected: it is built around giving the model tools and letting it
  drive, which is the opposite of the data-plane ownership decision above. It also does not
  change the billing question.
- **Requiring the operator to install Claude Code themselves.** Rejected: an unpinned,
  self-updating CLI on `PATH` would break the login scrape without warning, and the daemon runs
  as its own user with no interactive session to fix it.

## Consequences

- **We accept a screen-scrape.** `claude setup-token` is an Ink TUI with no flags and no JSON
  output, so obtaining a token interactively requires a PTY and text parsing. This is the
  single most fragile component in the system, and the entire release plan is arranged around
  it: static token submission ships first and always works, PTY auto-drive lands last, defaults
  off, and falls back to static submission on any failure.
- **We accept a silent-billing hazard.** Claude Code's credential precedence puts the
  settings-file key `apiKeyHelper` *above* `CLAUDE_CODE_OAUTH_TOKEN`, so a scrubbed environment
  is not sufficient protection. Mitigated by `--setting-sources ''` on every invocation plus an
  assertion that `authMethod == "oauth_token"`, with a dedicated regression test. See
  `.agents/rules/every-spawned-claude-process-is-credential-isolated.md`.
- **We accept 320 MB per installed version** (the `linux-arm64` binary), which is material on a
  Raspberry Pi's SD card. Retention is capped at two versions. Claude Code also wants 4 GB+ of
  RAM, so the Pi model has to be checked before deploying.
- Verification cannot use the obvious signals: `claude auth status --json` returns
  `loggedIn: true` for a bogus token, and `claude -p` returns `subtype: "success"` alongside
  `is_error: true`. Verification keys on `is_error`.
- Upgrading the pinned Claude Code version is a deliberate, tested change — it can invalidate
  the golden PTY transcripts — not a routine dependency bump.
