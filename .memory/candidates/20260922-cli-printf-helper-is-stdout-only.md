---
about: the shared `printf` helper in the cli package writes only to stdout, so any text that must not appear in a --json response has to bypass it
saw:
  - source/daemon/internal/cli/root.go
  - source/daemon/internal/cli/config.go
---

- `printf(cmd, format, args...)` in root.go always writes to `cmd.OutOrStdout()`. It has no
  stderr variant, so a caller printing an unconditional message alongside a `--json` branch (a
  note, a warning) has to write to `cmd.ErrOrStderr()` directly rather than reach for `printf` —
  otherwise the note lands after the JSON document on the same stream and a machine reader
  (`jq`, `json.Unmarshal`) fails on the trailing text.
- `config.go`'s `runConfigSet`/`runConfigReset` hit exactly this: an unconditional
  `liveDaemonNote` was originally sent through `printf` regardless of `--json`, corrupting the
  JSON stream. A `quick`-panel review caught it; the fix writes the note with
  `fmt.Fprint(cmd.ErrOrStderr(), liveDaemonNote)` instead.
