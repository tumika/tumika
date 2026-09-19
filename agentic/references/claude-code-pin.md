# Claude Code facts that are load-bearing

tumika drives a **pinned** Claude Code build, never whatever is on `PATH`. The exact version is
the compile-time constant `buildinfo.PinnedClaudeCodeVersion`, which is the single source of
truth — no document restates the number. The pin moves on a regular cadence, and a version
copied into prose is a version that goes stale.

The facts below were probed against the real CLI, and several contradict the obvious
implementation:

- `claude setup-token` prints a ~1-year OAuth token to the terminal and **saves it nowhere**.
  It is an Ink TUI with no flags, so capturing it requires a PTY and text parsing. This is the
  only screen-scrape in the system.
- `claude auth status --json` returns `loggedIn: true` **for a bogus token**. It is useful
  only for reading `authMethod` / `apiKeySource`.
- `claude -p` with a bad token returns `subtype: "success"` **and** `is_error: true,
  api_error_status: 401`. Verification keys on `is_error`, **never** `subtype`.
- Credential precedence puts `apiKeyHelper` — a *settings-file* key, not an env var — **above**
  `CLAUDE_CODE_OAUTH_TOKEN`. Scrubbing the environment is not sufficient. See
  `agentic/rules/every-spawned-claude-process-is-credential-isolated.md`.
- Claude Code auto-updates itself; that must be disabled (`DISABLE_AUTOUPDATER=1`), because an
  auto-update would silently break the login scrape.
- `--bare` does not read `CLAUDE_CODE_OAUTH_TOKEN`. Never pass it.

## Bumping the pin

Moving to a newer Claude Code is **routine and expected** — it is part of the ordinary update
cycle, not a rare event. What is not routine is doing it blind: the pin's whole purpose is that
the login parser was written against a build we have actually observed. So a bump is one commit
that changes `buildinfo.PinnedClaudeCodeVersion` **and** re-establishes that claim:

1. Install the new version and re-capture the `setup-token` PTY transcript into `testdata/`.
2. Re-run the transcript-driven parser tests against it. A changed auth-URL prefix or paste
   prompt shows up here, which is the point.
3. Re-run the two-stage `Verify` against a real credential.

If the transcripts still match, the bump is boring — which is the intended outcome most of the
time. If they do not, the parser changes in the same commit as the pin, so a released binary and
the TUI it parses are never out of step.
