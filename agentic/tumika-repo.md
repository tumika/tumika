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
go run ./source/daemon/cmd/tumika        # run the CLI locally

# First run: the daemon refuses to serve without an API token.
go run ./source/daemon/cmd/tumika token rotate   # mint one, printed once
go run ./source/daemon/cmd/tumika serve          # run the daemon in the foreground

# Release build dry-run (produces dist/):
go run github.com/goreleaser/goreleaser/v2@latest release --config source/daemon/.goreleaser.yml --snapshot --clean
```

- Module path: `github.com/tumika/tumika/source/daemon`. Go directive: `go 1.26`.
- `sqlc` is a build-time tool, deliberately **not** a module dependency — its own
  dependency tree would otherwise enter ours. Install the version CI pins:
  `go install github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1`.
- **All Go source lives under `/source`.** The `go.mod` stays at the repo root, so package
  paths are `github.com/tumika/tumika/source/daemon/internal/...` and goreleaser's `main:` is
  `./source/daemon/cmd/tumika`. This is valid Go — `internal/` visibility is scoped to its own
  parent, and `source/` contains all of our code.

## Layout

```
source/daemon/cmd/tumika/                      # thin main: version var + cobra Execute
source/daemon/internal/cli/                    # cobra commands (serve install status update token login config …)
source/daemon/internal/daemon/                 # composition root: wiring, runner supervision, shutdown
source/daemon/internal/api/                    # LAYER 1 — ServeMux routing, middleware, DTOs, SSE
source/daemon/internal/service/                # LAYER 2 — business logic + transaction boundaries
source/daemon/internal/repository/             # LAYER 3 — data access
source/daemon/internal/repository/sqlite/      #   sqlc-generated code + hand-written wrappers
source/daemon/internal/repository/queries/     #   sqlc input (*.sql)
source/daemon/internal/repository/migrations/  #   goose migrations, //go:embed
source/daemon/internal/runner/                 # supervised long-lived processes (Start/Stop)
source/daemon/internal/domain/                 # shared types; imports nothing of ours
source/daemon/internal/platform/provider/      # provider interfaces + registry
source/daemon/internal/platform/provider/claudecode/
source/daemon/internal/platform/provider/anthropicapi/
source/daemon/internal/platform/tokencustody/  # stores the minted API token in the platform keychain (macOS)
source/daemon/internal/platform/secrets/       # Sealer (AES-256-GCM) + env / keychain / file key custody
source/daemon/internal/platform/servicemgr/    # ServiceManager + launchd / systemd drivers
source/daemon/internal/platform/release/       # ReleaseSource (self-update)
source/daemon/internal/platform/paths/         # filesystem layout resolution
source/daemon/internal/platform/logging/       # slog setup + secret redaction handler
source/daemon/internal/platform/buildinfo/     # version/commit/date, injected at build time
source/desktop/                         # macOS tray app (Tauri: Rust core + React popover); not Go
deploy/Dockerfile                       # shipped image (tumika as PID 1)
deploy/verify-image.sh                  # exercises the shipped image, not just its build
deploy/testharness/Dockerfile           # CI-only: debian + systemd, exercises `tumika install`
deploy/testharness/verify.sh            # the linux-install gate
docs/adr/                               # architecture decision records
agentic/rules/                          # one prescriptive rule per file
agentic/references/                     # deep-dive notes, read on demand — see References
source/daemon/.golangci.yml                           # lint config (v2 schema) — depguard enforces layering
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

These are not aspirations. `depguard` in `source/daemon/.golangci.yml` fails the build on a forbidden
import, so a layering violation is a red pipeline rather than a review argument:

| Layer | May not import |
|---|---|
| `api` | `repository`, in any form |
| `service` | `repository/sqlite`, `api` — it takes repository *interfaces*, never an implementation |
| `repository` | `service`, `api`, `runner` |
| `runner` | `repository`, `api` |
| `platform` | `service`, `repository`, `api`, `runner` |
| `domain` | anything but the standard library and itself |

`daemon` is the exception, and that is the point: it is the composition root, so it is the only
package that may name a concrete implementation.

## Rules

This repo has prescriptive rules in `agentic/rules/`. **Read every file in that directory before making changes here, and follow each rule strictly.**
Each file contains one rule. New rules go in that directory — one file per rule, kebab-case filename matching the rule's intent.

## Desktop tray app

`source/desktop/` is a macOS-only Tauri app and a client of the daemon exactly as the CLI is:
it holds no state. Its Rust core reads the API token from the login Keychain via
`/usr/bin/security` and polls `/v1/health`; the React popover renders the result. It is not
part of the Go module, the daemon image, or the release archives. Toolchain and commands are in
`source/desktop/README.md`; it has its own lint (`eslint`) and licence check (`cargo-deny`) and
its own CI job.

## References

Each file below holds the detail for one area, and carries facts that are not restated here.
**Read the matching reference before changing the area it describes.**

- `agentic/references/provider-model.md` — capability declaration and credential storage order;
  read before touching `platform/provider/` or `ProviderService`.
- `agentic/references/claude-code-pin.md` — the probed Claude Code CLI facts and the pin-bump
  procedure; read before changing the claudecode driver or moving the pin.
- `agentic/references/http-api.md` — bearer-token policy and middleware order; read before
  adding a route or touching middleware.
- `agentic/references/credential-sealing.md` — envelope encryption and key custody; read before
  touching `platform/secrets` or `tokencustody`.
- `agentic/references/service-management.md` — what install must do and what `status` must never
  report; read before changing `platform/servicemgr` or the install command.
- `agentic/references/containers-and-ci.md` — the two images and the no-cgo enforcement; read
  before editing `deploy/` or a workflow.
- `agentic/references/self-update.md` — the update's two halves and the four orderings that
  carry its safety; read before touching `UpdateService` or `platform/release`.
- `agentic/references/releasing.md` — the tag-driven release and the draft promote gate; read
  before changing goreleaser config or the release workflow.

## Conventions

- **Binaries must stay fully self-contained.** Every build sets `CGO_ENABLED=0`, which is what
  makes cross-compiling for `linux/arm64` free. This is why the SQLite driver is
  `modernc.org/sqlite` (pure Go). **Do not introduce cgo dependencies.**
- **Version injection:** `source/daemon/cmd/tumika` exposes `var version = "dev"`, overridden at
  release via `-ldflags "-X main.version=<tag>"`. Keep that variable name and package stable —
  the updater and the `dev`-build short-circuit both depend on it.
- **goreleaser and golangci-lint both use the v2 config schema.** In golangci-lint v2,
  `gosimple` is part of `staticcheck` — do not add it as a separate linter (it will error).
- Lint must pass with zero issues; `errcheck` is on, so check returned errors.
- `CLAUDE.md` and `AGENTS.md` are **generated by the agentic toolkit** — do not author or edit
  them. Edit this file (`agentic/tumika-repo.md`), `agentic/rules/` and `agentic/references/`
  instead, then `agtk sync`.

## Boundaries

- **Always:** run `go build ./...`, `go test -race ./...`, `golangci-lint run ./...` and
  `bulwark scan` before proposing a PR; write a goose migration and regenerate sqlc in the same
  commit as any schema change; keep `source/daemon/.golangci.yml`'s depguard rules in step with
  `agentic/rules/`; write commit messages **and pull request titles** as
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

**Reviewing a branch:** `cd` to that branch's worktree first. This session's
working directory is often an older one, and a review run there silently reviews
long-merged code — it has produced three rounds of stale findings.
