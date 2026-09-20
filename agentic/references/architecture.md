# Architecture and layout

```
source/daemon/cmd/tumika/                      # thin main: version var + cobra Execute
source/daemon/cmd/tumika-bom/                  # CI-only: builds, signs and self-verifies the published BOM tree
source/daemon/internal/bomgen/                 # pure BOM generation from the releases list; tumika-bom signs it
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
source/daemon/internal/platform/release/       # release Source (channel heads, signed BOM verification, asset fetch)
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
.github/workflows/{ci,ci-build,release,edge,publish-pages}.yml
scripts/                                # install-daemon.sh, site assembly, edge version/prune helpers
```

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
