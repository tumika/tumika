# tumika — agent guide

tumika (Swahili: *be useful, be in service*) is a self-hostable personal assistant daemon that
runs deterministic workflows on a schedule or on events; the first will be inbox triage. One
statically-linked binary installs and supervises itself on macOS and Linux, serves a
token-authenticated HTTP API, persists to SQLite, updates itself, and installs + authenticates
LLM providers. This branch builds **foundations only** — no workflow engine, no UI, no connectors.

Two constraints drive the whole design and explain most of what looks unusual here. **Cost:**
Anthropic API billing is prohibitive for an agent running every few hours, so tumika drives the
vendored `claude` CLI against a Claude *subscription* OAuth token rather than calling the API;
API-key providers stay first-class for anyone who prefers them. **Data-plane ownership:** tumika
fetches and filters data itself and hands the model a curated payload, so the model never gets
direct access to the mailbox.

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
go run ./source/daemon/cmd/tumika serve  # run the daemon in the foreground
```

First run, the release dry-run, the module path and the `sqlc` version pin are in `agentic/references/commands.md`.

## Rules

This repo has prescriptive rules in `agentic/rules/`. **Read every file in that directory before making changes here, and follow each rule strictly.**
Each file contains one rule. New rules go in that directory — one file per rule, kebab-case filename matching the rule's intent.

## Desktop tray app

`source/desktop/` is a macOS-only Tauri app and a client of the daemon exactly as the CLI is: it
holds no state. Its Rust core reads the API token from the login Keychain via `/usr/bin/security`
and polls `/v1/health`; the React popover renders the result. It is not part of the Go module,
the daemon image, or the daemon's release archives; a release ships it as its own `desktop`
component (`docs/adr/0010-the-desktop-app-is-a-release-component.md`). Toolchain and commands are in
`source/desktop/README.md`; it has its own lint (`eslint`), licence check (`cargo-deny`) and CI job.

## References

Each file holds the detail for one area and carries facts not restated here. **Read the matching reference before changing the area it describes.**

- `agentic/references/architecture.md` — the layers, the repo layout tree and the depguard table; read before adding a package or moving code between layers.
- `agentic/references/commands.md` — first run, release dry-run, module path and tool pins; read when the block above is not enough.
- `agentic/references/provider-model.md` — capability declaration and credential storage order; read before touching `platform/provider/` or `ProviderService`.
- `agentic/references/claude-code-pin.md` — the probed Claude Code CLI facts and the pin-bump procedure; read before changing the claudecode driver or moving the pin.
- `agentic/references/http-api.md` — bearer-token policy and middleware order; read before adding a route or touching middleware.
- `agentic/references/credential-sealing.md` — envelope encryption and key custody; read before touching `platform/secrets` or `tokencustody`.
- `agentic/references/service-management.md` — what install must do and what `status` must never report; read before changing `platform/servicemgr` or the install command.
- `agentic/references/containers-and-ci.md` — the two images and the no-cgo enforcement; read before editing `deploy/` or a workflow.
- `agentic/references/self-update.md` — the update's two halves and the four orderings that carry its safety; read before touching `UpdateService` or `platform/release`.
- `agentic/references/releasing.md` — the tag-driven release and the draft promote gate; read before changing goreleaser config or the release workflow.

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

One session, one `gt wt add <type/name>` worktree; never use raw `git worktree` or edit
inside `.bare/`.

**Reviewing a branch:** `cd` to that branch's worktree first. This session's
working directory is often an older one, and a review run there silently reviews
long-merged code — it has produced three rounds of stale findings.
