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

# First run: the daemon refuses to serve without an API token.
go run ./source/cmd/tumika token rotate   # mint one, printed once
go run ./source/cmd/tumika serve          # run the daemon in the foreground

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
source/internal/platform/secrets/       # Sealer (AES-256-GCM) + env / keychain / file key custody
source/internal/platform/servicemgr/    # ServiceManager + launchd / systemd drivers
source/internal/platform/release/       # ReleaseSource (self-update)
source/internal/platform/paths/         # filesystem layout resolution
source/internal/platform/logging/       # slog setup + secret redaction handler
source/internal/platform/buildinfo/     # version/commit/date, injected at build time
deploy/Dockerfile                       # shipped image (tumika as PID 1)
deploy/verify-image.sh                  # exercises the shipped image, not just its build
deploy/testharness/Dockerfile           # CI-only: debian + systemd, exercises `tumika install`
deploy/testharness/verify.sh            # the linux-install gate
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

This repo has prescriptive rules in `.agents/rules/`. **Read every file in that directory before making changes here, and follow each rule strictly.**
Each file contains one rule. New rules go in that directory — one file per rule, kebab-case filename matching the rule's intent.

## Provider model

A provider is an LLM backend. Two ship in this branch:

| ID | Kind | Auth methods | Installs a binary |
|---|---|---|---|
| `claude-code` | `cli` | `manual_token` | yes (vendored `claude`) |
| `anthropic-api` | `http` | `api_key` | no |

`claude-code` will also offer `interactive_cli` once the PTY login lands. It does **not**
declare it today, and must not: the registry validates the descriptor against the interfaces
actually implemented, so declaring a method ahead of its implementation stops the daemon at
startup. The absence is discoverable by type assertion, which is what will let the login
endpoint refuse with `400 interactive_auth_unsupported` when it lands. **There is no
`POST /v1/providers/{id}/login` route yet** — that path answers 404 today, not 400.

Providers **declare their capabilities by which interfaces they implement**, and the registry
discovers them by type assertion. `Provider` and `HealthChecker` are mandatory;
`StaticAuthenticator`, `InteractiveAuthenticator` and `Installer` are optional. Clients read
`requires_interactive_auth` from the descriptor to decide whether to `PUT` a secret or drive
the login-session endpoints.

`anthropic-api` exists precisely so the abstraction is not silently Claude-CLI-shaped: it
implements neither `Installer` nor `InteractiveAuthenticator`.

**The registry validates the correspondence at construction**, so a driver whose descriptor
disagrees with the interfaces it implements stops the daemon at startup rather than producing a
client that offers a flow the daemon rejects. The compiler cannot check that, which is why the
registry does. Every driver must also pass the shared suite in
`platform/provider/providertest` — written once, run against every implementation, so a second
driver cannot quietly diverge from the first.

**Credentials are stored before they are verified, deliberately.** Sealing and insertion happen
in one transaction; verification is a network call made holding *no* transaction; the verdict
lands in a second. Verifying inside the transaction would hold SQLite's single write lock across
a network call, so a hanging provider would block every other write in the daemon. This is why
the schema has an `unverified` status at all.

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

## HTTP API

Every route is behind a bearer token — there are no exemptions, including
`/v1/health`. Only the token's SHA-256 is stored, so a lost token is replaced
(`tumika token rotate`), never recovered, and the daemon refuses to start rather
than listen unauthenticated.

Middleware, outermost first:

| Order | Middleware | Why there |
|---|---|---|
| 1 | recovery | outermost, so a panic *anywhere* below becomes a 500 rather than a dropped connection |
| 2 | logging | above the security checks, so refusals are logged — a burst of 401s is what a probe looks like from the inside |
| 3 | Host allowlist | the DNS-rebinding defence; runs before auth because it exists to turn away unauthenticated probes. Literal IPs pass: they cannot be rebound |
| 4 | Origin check | no `Origin` (curl, the CLI) passes; a browser origin must be allowed. No CORS headers are set anywhere |
| 5 | bearer token | constant-time compare against the stored hash |

Note this differs from the plan's literal ordering, which put recovery and
logging innermost. Placed there, recovery would not cover a panic in the layers
above it and logging would never see a rejected request — both of which defeat
the point of having them.

## Credential sealing

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

**There is no fallback off the Keychain on macOS, deliberately.** A locked
keychain or a denied prompt fails the daemon closed. Falling back to a file would
find no key, mint a fresh one, start cleanly — and be unable to open a single
existing credential, while anything re-submitted during that run got sealed under
the new key and orphaned on the next successful start.

## Service management

`tumika install` sets the daemon up under the platform's supervisor — a systemd
system unit on Linux, a LaunchAgent on macOS — and `uninstall / start / stop /
status` drive it from there. Neither driver is behind a build tag: both compile
everywhere and only `servicemgr.New` consults `runtime.GOOS`, so the Linux unit
rendering and command sequencing are testable on a Mac.

Six things the Linux install must do, every one of which was a silent failure
first — the install printed success and the service did not run:

- **Use `paths.SystemHome`, not the XDG per-user default.** `sudo tumika install`
  resolves *root's* `HOME`, so the unit named `/root/.local/state/tumika`, which
  the unprivileged service account cannot traverse.
- **Hand `TUMIKA_HOME` to the service account.** The layout is created `0700` by
  root and the unit runs as `tumika`.
- **Create the service account BEFORE probing key custody.** The handover probe
  runs a transient unit *as* that account, so probing first made `systemd-run`
  fail to resolve the user on every first install — the probe reported "no
  handover", the freshly sealed key was deleted, and the host dropped to the file
  key permanently.
- **Mint an API token before starting**, and print it as soon as it exists. The
  daemon refuses to serve without one; and the hash is stored the moment it is
  minted, so an install that fails afterwards would otherwise leave a daemon with
  a token nobody has ever seen.
- **Prove the credential handover** before writing `LoadCredentialEncrypted=`.
- **`systemctl enable` then `restart`, never `enable --now`.** `start` is a no-op
  on an active unit, so the documented upgrade path left the daemon running the
  old binary with the old unit settings.

Two things `status` must never do:

- **Report `activating` as running.** That is what a `Restart=always` crash loop
  reports *forever* — it never reaches `failed`, because the supervisor keeps
  trying. It maps to `starting`, and to `failed` once `NRestarts` is above zero.
- **Assume a LaunchAgent is enabled because a plist exists.** `launchctl
  print-disabled` is the answer.

The whitespace/`%` rule lives in the systemd driver, not in `Config.Validate`:
macOS's own default home is `~/Library/Application Support/tumika`, and refusing
a space would break every Mac install to protect Linux.

`deploy/testharness/verify.sh` runs the whole thing under a real systemd in a
podman container: install, ownership, the unit systemd actually accepts,
`/v1/health` through the supervised daemon, `Restart=always` recovery from
`SIGKILL`, a second install that must land on a **new pid**, a system install
with container detection **off**, a crash loop that must not report as running,
and an uninstall that leaves the database. Unit tests check what is rendered and
sequenced; only this checks whether systemd agrees.

Note what the container itself hides: `InContainer()` forces the system home, so
the per-user-home bug was invisible until the harness started running an install
with `TUMIKA_CONTAINER=0`. A harness has blind spots of its own, and they are
worth writing down when found.

## Containers

Two images, and they are not interchangeable (ADR: D9):

- **`deploy/Dockerfile`** — what a user runs. One process, `tumika` as PID 1, logs
  on stdout, restart policy left to the orchestrator. No service manager, no
  `tumika install`. Runs as an unprivileged account with `/var/lib/tumika` as the
  only volume.
- **`deploy/testharness/Dockerfile`** — CI only. A real systemd, so `tumika
  install` is *exercised* rather than asserted about.

Both are exercised by a script rather than merely built, because a Dockerfile
that parses proves nothing. `verify-image.sh` checks that the binary runs on that
base, that an unprivileged account can write a fresh volume, that the documented
two-step first run works (`token rotate`, then `serve` — the daemon refuses to
serve without a token), that `/v1/health` answers through a published port, that
the token never reaches the logs, and that state survives a restart.

The API binds loopback by default, which inside a container means unreachable.
That is deliberate — it carries a bearer token in clear text — so publishing a
port also means passing `--listen`, and the daemon logs a warning when you do.

## CI

`cross-compile` builds all four release targets. Nothing else does: `build &
test` compiles for whatever the runner is, and tumika is written for a Pi.

Its `no cgo` step is the real enforcement of the no-cgo invariant. `.golangci.yml`
has a `nocgo` depguard rule that **documents intent and enforces nothing** —
depguard never sees `import "C"`, in either cgo mode. And
`CGO_ENABLED=0 go build ./...` does not catch it either: build constraints
exclude the file, the pattern match skips the package, and the build exits zero.
Both were verified by adding a cgo package. `go list` reporting a non-empty
`CgoFiles` is what actually fires.

**Reviewing a branch:** `cd` to that branch's worktree first. This session's
working directory is often an older one, and a review run there silently reviews
long-merged code — it has produced three rounds of stale findings.

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
