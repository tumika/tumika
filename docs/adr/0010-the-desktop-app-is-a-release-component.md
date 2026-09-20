---
status: accepted
date: 2026-09-20
---

# The desktop app is a release component, signed for its own updater

ADR-0006 defines a release as a label over components that each keep their own semver, and
ADR-0009 how the documents are generated and how edge is cut. This records how the macOS tray
app becomes the second component of a release, what its updater verifies, and how the build is
kept away from the signing keys.

## Decisions

- **`release.yaml` stays flat.** The desktop component is one more `<name>: <semver>` line
  under `components:`. Every reader of the file is a line-based awk or regex script
  (`release-label.sh`, `release-component-version.sh`, `validate-release.sh`) or the Go
  parser that mirrors them, so a nested map, such as a component with its own platform list,
  breaks all of them at once.

- **An equal component version means the component is carried over.**
  `scripts/check-release-monotonic.sh` accepts a component version equal to the last published
  release's, refuses a lower one, and reports the components that differ as `changed=` (a step
  output, and a line on stdout). Only the changed components are built. `bomgen` gives an
  unchanged component an entry whose assets are the earlier release's, named by `from_release`,
  so the same bytes are never republished under a second component version. A component named
  for the first time counts as changed. Equality is what "unchanged" means because carrying a
  component over is only expressible by repeating its version.

- **Asset names are a contract, and Tauri's own names are renamed to it.** A component's
  assets are `<binary>_<component version>_<goos>_<goarch><extension>`; the desktop is
  `tumika-desktop_<component version>_<goos>_<goarch>.app.tar.gz` and its `.sig`. Tauri writes
  `Tumika.app.tar.gz`, so the workflow renames it and `scripts/verify-desktop-assets.sh`
  asserts the result. Platform keys in the BOM are `darwin_arm64` and `darwin_amd64`. Four
  names begin `tumika` and only the ending tells them apart: the daemon's raw binary (no
  extension, which the daemon rule matches), its `.tar.gz`, the app's `.app.tar.gz` and that
  archive's `.sig`. The daemon rule takes the whole remainder as a dot-free platform, so it
  never matches the `.tar.gz`; the desktop rule requires `.app.tar.gz`, so it never matches the
  `.sig`. A name the generator cannot read leaves the component "not built by this release",
  not an error, which is why the assertion has to be made at build time.

- **The Tauri updater signature is a field of the BOM asset.** An asset carries an optional
  `signature`, the text of the `<name>.sig` beside the archive. The document's own signature
  covers it, so the updater reads a signature it can trust from the document it already
  verified. A component published with a signature rule refuses an archive whose `.sig` is
  missing. Daemon-only BOMs are unchanged: the field is omitted when empty.
  Rejected: publishing the `.sig` as a sibling asset for the updater to fetch. It is a second
  unauthenticated fetch from the host whose trustworthiness the BOM exists to remove.

- **The Apple switch is all six `APPLE_*` secrets or none.** With `APPLE_CERTIFICATE`,
  `APPLE_CERTIFICATE_PASSWORD`, `APPLE_SIGNING_IDENTITY`, `APPLE_ID`, `APPLE_PASSWORD` and
  `APPLE_TEAM_ID` all present, the app is signed with a Developer ID and notarized. With none,
  `tauri.conf.json`'s `signingIdentity` of `"-"` yields an ad-hoc signature; no Apple Developer
  account exists today. Anything between fails the job, because a half-configured setup that
  fell back to ad-hoc would publish an unsigned app under a release that looks signed.

- **Edge builds the desktop app without a key near branch code.** The `desktop` job of
  `edge.yml` runs the branch's pnpm scripts, `build.rs` and `beforeBuildCommand` under
  `contents: read` and no secret. It builds with `createUpdaterArtifacts` off (the committed
  value is on) and packs the bundle into the named archive by hand. A separate `sign` job runs
  `main`'s checkout and `main`'s pinned Tauri CLI, downloads the archives as data and runs
  `tauri signer sign` with the updater key; it executes nothing from the branch. `publish` then
  checks every file name against `scripts/edge-check-artifact.sh`, a closed allow-list that
  admits the daemon's assets and both desktop archives with their signatures, all at the edge
  component versions. Rejected: the key in the build job, since `pnpm build` would run beside
  the one key every installed app verifies updates against; and carrying the desktop over on
  edge, since edge exists to put a branch on a machine and the app is part of the branch.

- **The release workflow separates building, writing and promoting.** The `desktop` matrix
  (one leg per platform) holds the signing secrets and a read-only token, and uploads its
  archives as an artifact. `finalize` holds `contents: write` and no signing secret: it creates
  the draft when `goreleaser` was skipped, attaches the desktop assets, and appends their
  digests to `checksums.txt`, which `bomgen` reads every digest from and goreleaser writes for
  the daemon alone. `promote` is a job of its own, so the release becomes public only after
  every component is attached. `finalize` and `promote` run under `!cancelled() && !failure()`,
  which tolerates a skipped needed job (an unchanged component) and stops on a failed or
  cancelled one. A failed leg therefore leaves a draft, never a published release.

- **The desktop job asserts, and does not stamp, the component version.** The desktop
  component version has four committed copies: `tauri.conf.json`, `Cargo.toml`, `Cargo.lock`
  and `package.json`. The release job runs `scripts/desktop-version.sh --check`, so a change to
  `release.yaml` moves the four with it in the same commit. Edge stamps them
  (`scripts/desktop-version.sh <edge component version>`) because its component version is
  suffixed and derived on the runner.

## Setup the workflows cannot create

- A Tauri updater keypair, generated with `pnpm tauri signer generate`. Its public key is
  committed in `tauri.conf.json` (`plugins.updater.pubkey`). Its private key is the secret
  `TAURI_SIGNING_PRIVATE_KEY` (the key's contents, not a path), with
  `TAURI_SIGNING_PRIVATE_KEY_PASSWORD` beside it. Losing the private key means a new public key
  has to reach every installed app, and an installed app can only be updated by a build its old
  key signed, so those apps are reinstalled by hand.
- Optionally the six `APPLE_*` secrets, all or none.
- `createUpdaterArtifacts` is on in `tauri.conf.json` and needs that pubkey to build, so a
  build without it (CI's `ci-build.yml`, edge's `desktop` job) overrides it off.

## Consequences

- A release that changes only the daemon builds no desktop app and its BOM entry points at the
  earlier release's archives.
- A desktop leg that fails leaves a draft release; the daemon assets it holds are inert until
  a re-run completes it.
- Ad-hoc signing means macOS shows its unidentified-developer prompt on first launch of a
  downloaded app; the updater's signature check does not depend on it.
