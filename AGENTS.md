# tumika — agent guide

tumika (Swahili: *be useful, be in service*) is a self-hostable personal assistant daemon. It
runs deterministic workflows on a schedule or on events; the first will be inbox triage. This
repository builds a single statically-linked binary, `tumika`, which installs and supervises
itself on macOS and Linux, serves a token-authenticated HTTP API, persists to SQLite, updates
itself, and installs + authenticates LLM providers.

Two constraints drive the whole design and explain most of what looks unusual here:

1. **Cost.** Anthropic API billing is prohibitive for an agent running every few hours, so
   tumika drives the vendored `claude` CLI against a Claude *subscription* OAuth token rather
   than calling the API. API-key providers stay first-class for anyone who prefers them.
2. **Data-plane ownership.** tumika fetches and filters data itself and hands the model a
   curated payload. The model never gets direct access to the mailbox.

The current branch builds **foundations only** — no workflow engine, no UI, no connectors.

## Commands

```sh
go build ./...                    # build
go test -race ./...               # run tests (race detector on, as CI does)
golangci-lint run ./...           # lint — must be clean before a PR; enforces the layering rules
bulwark scan                      # gosec + govulncheck + semgrep, exactly as CI runs them
bulwark coverage                  # diff coverage against the cached baseline (the CI gate)
sqlc generate                     # regenerate repository/sqlite from queries/ + migrations/
sqlc diff                         # fail if the committed generated code is stale (the CI gate)
go run ./source/cmd/tumika        # run the CLI locally

# Release build dry-run (produces dist/):
go run github.com/goreleaser/goreleaser/v2@latest release --snapshot --clean
```

- Module path: `github.com/tumika/tumika`. Go directive: `go 1.26`.
- `sqlc` is a build-time tool, deliberately **not** a module dependency — its own
  dependency tree would otherwise enter ours. Install the version CI pins:
  `go install github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1`.
- **All Go source lives under `/source`.** The `go.mod` stays at the repo root, so package
  paths are `github.com/tumika/tumika/source/internal/...` and goreleaser's `main:` is
  `./source/cmd/tumika`. This is valid Go — `internal/` visibility is scoped to its own
  parent, and `source/` contains all of our code.

## Layout

```
source/cmd/tumika/                      # thin main: version var + cobra Execute
source/internal/cli/                    # cobra commands (serve install status update token login config …)
source/internal/daemon/                 # composition root: wiring, runner supervision, shutdown
source/internal/api/                    # LAYER 1 — ServeMux routing, middleware, DTOs, SSE
source/internal/service/                # LAYER 2 — business logic + transaction boundaries
source/internal/repository/             # LAYER 3 — data access
source/internal/repository/sqlite/      #   sqlc-generated code + hand-written wrappers
source/internal/repository/queries/     #   sqlc input (*.sql)
source/internal/repository/migrations/  #   goose migrations, //go:embed
source/internal/runner/                 # supervised long-lived processes (Start/Stop)
source/internal/domain/                 # shared types; imports nothing of ours
source/internal/platform/provider/      # provider interfaces + registry
source/internal/platform/provider/claudecode/
source/internal/platform/provider/anthropicapi/
source/internal/platform/secrets/       # Sealer + keychain / systemdcreds / file backends
source/internal/platform/servicemgr/    # ServiceManager + launchd / systemd drivers
source/internal/platform/release/       # ReleaseSource (self-update)
source/internal/platform/paths/         # filesystem layout resolution
source/internal/platform/logging/       # slog setup + secret redaction handler
source/internal/platform/buildinfo/     # version/commit/date, injected at build time
deploy/Dockerfile                       # shipped image (tumika as PID 1)
deploy/testharness/Dockerfile           # CI-only: debian + systemd, exercises `tumika install`
docs/adr/                               # architecture decision records
.agents/rules/                          # one prescriptive rule per file
.golangci.yml                           # lint config (v2 schema) — depguard enforces layering
.github/workflows/{ci,release}.yml
```

## Architecture

```
CLI ──┐
      ├──► API ──► SERVICE ──► REPOSITORY ──► SQLite
RUNNER┘              │
                     └──────► PLATFORM (provider, secrets, servicemgr, release)
```

- **API** is transport only: decode, call exactly one service method, encode.
- **SERVICE** owns all business logic and transaction boundaries. It is the only layer that
  may call repositories.
- **REPOSITORY** is data access only. It returns domain types, never `*sql.Rows`.
- **RUNNER** is a supervised long-lived process (`Start(ctx)` / `Stop(ctx)`) that depends on
  services only.
- **PLATFORM** holds infrastructure abstractions. Services depend on platform *interfaces*;
  implementations are injected in `daemon`, the composition root.
- **DOMAIN** holds shared types. Every layer may import it; it imports nothing of ours.

Each repository has exactly one owning service:

| Service | Owns repositories |
|---|---|
| `ConfigService` | `ConfigRepository` |
| `ProviderService` | `ProviderRepository`, `CredentialRepository` |
| `LoginService` | `LoginSessionRepository` |
| `UpdateService` | `UpdateStateRepository` |

These are not aspirations. `depguard` in `.golangci.yml` fails the build on a forbidden
import, so a layering violation is a red pipeline rather than a review argument.

## Rules

This repo has prescriptive rules in `.agents/rules/`. **Read every file in that directory before making changes here, and follow each rule strictly.**
Each file contains one rule. New rules go in that directory — one file per rule, kebab-case filename matching the rule's intent.

## Provider model

A provider is an LLM backend. Two ship in this branch:

| ID | Kind | Auth methods | Installs a binary |
|---|---|---|---|
| `claude-code` | `cli` | `interactive_cli`, `manual_token` | yes (vendored `claude`) |
| `anthropic-api` | `http` | `api_key` | no |

Providers **declare their capabilities by which interfaces they implement**, and the registry
discovers them by type assertion. `Provider` and `HealthChecker` are mandatory;
`StaticAuthenticator`, `InteractiveAuthenticator` and `Installer` are optional. Clients read
`requires_interactive_auth` from the descriptor to decide whether to `PUT` a secret or drive
the login-session endpoints.

`anthropic-api` exists precisely so the abstraction is not silently Claude-CLI-shaped: it
implements neither `Installer` nor `InteractiveAuthenticator`.

## Claude Code facts that are load-bearing

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
  `.agents/rules/every-spawned-claude-process-is-credential-isolated.md`.
- Claude Code auto-updates itself; that must be disabled (`DISABLE_AUTOUPDATER=1`), because an
  auto-update would silently break the login scrape.
- `--bare` does not read `CLAUDE_CODE_OAUTH_TOKEN`. Never pass it.

### Bumping the pin

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

## Conventions

- **Binaries must stay fully self-contained.** Every build sets `CGO_ENABLED=0`, which is what
  makes cross-compiling for `linux/arm64` free. This is why the SQLite driver is
  `modernc.org/sqlite` (pure Go). **Do not introduce cgo dependencies.**
- **Version injection:** `source/cmd/tumika` exposes `var version = "dev"`, overridden at
  release via `-ldflags "-X main.version=<tag>"`. Keep that variable name and package stable —
  the updater and the `dev`-build short-circuit both depend on it.
- **goreleaser and golangci-lint both use the v2 config schema.** In golangci-lint v2,
  `gosimple` is part of `staticcheck` — do not add it as a separate linter (it will error).
- Lint must pass with zero issues; `errcheck` is on, so check returned errors.
- `CLAUDE.md` is **generated by the agentic toolkit** — do not author or edit it.
  `.agents/CODE-MAP.md` is maintained by the `code-explorer` agent — do not hand-write it.

## Boundaries

- **Always:** run `go build ./...`, `go test -race ./...`, `golangci-lint run ./...` and
  `bulwark scan` before proposing a PR; write a goose migration and regenerate sqlc in the same
  commit as any schema change; keep `.golangci.yml`'s depguard rules in step with
  `.agents/rules/`; write commit messages **and pull request titles** as
  [Conventional Commits](https://www.conventionalcommits.org/en/v1.0.0/) — PRs are squash-merged,
  so the title is the commit that lands on `main`.
- **Ask first:** changing the Go version, moving code out of `/source`, adding a third
  provider, altering the release archive layout (the raw-binary archive is what self-update
  fetches), or editing CI.
- **Never:** introduce cgo; commit `dist/`, a real credential, or a `tumika.db`; log or return
  a credential secret; skip the lint/test gates; merge a PR without being told to.

## Worktrees

This repo uses a bare-repo + typed-worktree layout managed by the `gt` CLI — one session, one
`gt wt add <type/name>` worktree; never use raw `git worktree` or edit inside `.bare/`. The
root `.envrc` scopes `GH_TOKEN` to the GitHub user that `gh` commands must run as.
