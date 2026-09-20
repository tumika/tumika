# Containers and CI

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

Its `no cgo` step is the real enforcement of the no-cgo invariant. `source/daemon/.golangci.yml`
has a `nocgo` depguard rule that **documents intent and enforces nothing** —
depguard never sees `import "C"`, in either cgo mode. And
`CGO_ENABLED=0 go build ./...` does not catch it either: build constraints
exclude the file, the pattern match skips the package, and the build exits zero.
Both were verified by adding a cgo package. `go list` reporting a non-empty
`CgoFiles` is what actually fires.

## Release builds

`release.yml`'s image job takes its build args and tags from `release.yaml`, not
from the git tag. `VERSION` is the daemon component's semver
(`scripts/release-component-version.sh daemon`) and `RELEASE` is the label
(`scripts/release-label.sh`); each script fails the job rather than yielding an
empty value that would override the Dockerfile's defaults. The image is tagged
with the component version and with the label, both as `type=raw`, because the
tag is a zero-padded CalVer that `type=semver` cannot parse. `:latest` is a
third `type=raw` tag enabled only when the label carries no `-beta.`.

`ci-build.yml`'s snapshot release build exports `TUMIKA_DAEMON_VERSION` only.
`.goreleaser.yml` fails without it, and leaving `TUMIKA_RELEASE` unset keeps the
snapshot on the `dev` label that `verify-release-assets.sh` asserts.

## Publishing workflows

Two workflows beyond `release.yml` publish the release host (ADR-0009):

- **`publish-pages.yml`** builds the signed BOM tree with `tumika-bom`, assembles
  it with `scripts/assemble-site.sh`, and deploys it to Pages. It runs on
  `release: published`, weekly, on dispatch, and as a called workflow. Its top
  level is `contents: read`; only the `deploy` job holds `pages: write` and
  `id-token: write`, and it checks nothing out. Concurrency group `pages` is
  never cancelled, so one deploy runs at a time.
- **`edge.yml`** cuts an edge build on dispatch from any branch. Its top level is
  `contents: read`, the `build` job adds `contents: write`, and its concurrency
  group is per ref, never cancelled.

`release.yml` and `edge.yml` each end in a `pages` job that calls
`publish-pages.yml` (`uses:` plus `secrets: inherit`). The promote step publishes
with `GITHUB_TOKEN`, which raises no workflow event, so the trigger would not
fire on its own. A called workflow cannot hold more permission than its caller,
so each `pages` job grants `contents: read`, `pages: write` and `id-token: write`.

The shell fixture tests under `scripts/*_test.sh` are run by hand; no workflow in
`.github/` invokes them.
