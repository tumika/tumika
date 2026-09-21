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

`release.yml`'s jobs run in the order `gate`, then `goreleaser` and the
`desktop` matrix, then `finalize`, `promote`, `image` and `pages` (see
`releasing.md`); `goreleaser`, `desktop` and `image` run only when the gate's
`changed` output names their component. `desktop` holds the app updater's
signing secrets under a read-only token; `finalize` holds the write token and
no signing secret. ADR-0010 records why.

`release.yml`'s image job takes its build args and tags from `release.yaml`, not
from the git tag. `VERSION` is the daemon component's semver
(`scripts/release-component-version.sh daemon`) and `RELEASE` is the label
(`scripts/release-label.sh`); each script fails the job rather than yielding an
empty value that would override the Dockerfile's defaults. The image is tagged
with the component version and with the label, both as `type=raw`, because the
tag is a zero-padded CalVer that `type=semver` cannot parse. `:latest` is a
third `type=raw` tag enabled only when the label carries no `-beta.`.

`ci-build.yml`'s desktop build and `edge.yml`'s `desktop` job pass
`-c '{"bundle":{"createUpdaterArtifacts":false}}'`: the committed value is on,
which needs the updater private key.

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
- **`edge.yml`** is dispatched on `main` and builds whatever its `ref` input
  names, in four jobs after a `guard`. Its top level is `contents: read`; the
  `build` and `desktop` jobs that run the named ref's code stay at `contents:
  read` with no secret. `sign` runs `main`'s checkout with the updater key over
  the desktop archives, as data. Only `publish` — which runs `main`'s checkout —
  holds `contents: write` and `actions: write`. Concurrency group `edge`, never
  cancelled.

`release.yml` ends in a `pages` job that dispatches `publish-pages.yml` on `main`
(`gh workflow run`, `actions: write`). The promote step publishes with
`GITHUB_TOKEN`, which raises no workflow event, so the release trigger would not
fire on its own, though that token may still dispatch a workflow. The job does
not call the workflow with `uses:`: the `release-signing` environment secret
arrives empty in a called workflow's job, so the site build fails after the
release is already published. `edge.yml`'s `publish` job dispatches it the same
way, which also keeps the built branch's own `tumika-bom` source away from the
signing key: the signing job always runs `main`'s code. The signing key is a
secret of the `release-signing` environment, whose deployment-branch policy
admits only `main` and `v*.*.*` tags, so a run from any other branch cannot
resolve it. The environment cannot tell whether a `v*.*.*` tag was cut from
`main`, so a tag ruleset restricting who may create those tags is what closes
the tag path.

The branch being built never runs beside a write token or a signing key:
`edge.yml`'s `build` and `desktop` jobs hold `contents: read` and no credentials,
and hand their output on as artifacts (the desktop one through `sign`) whose file
names `scripts/edge-check-artifact.sh` validates
against a closed allow-list before anything is uploaded.

The shell fixture tests under `scripts/*_test.sh` are run by hand; no workflow in
`.github/` invokes them.
