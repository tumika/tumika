# Releasing

Edit `release.yaml` (the label and each component version), merge it to `main`,
then tag `v<label>` and push the tag. `release.yml` then: re-runs the full gate
(including the systemd install harness) on that exact commit, builds the
components whose version changed, publishes the release, and pushes a multi-arch
image to `ghcr.io/tumika/tumika`. ADR-0008 records why the tag names the release
and nothing else; ADR-0010 records the desktop component.

The job graph: `gate` → `goreleaser` (daemon changed) and `desktop` (desktop
changed, one leg per platform) → `finalize` → `promote` → `image` (daemon
changed) → `pages`. `goreleaser` creates the draft release; `desktop` holds the
signing secrets and a read-only token; `finalize` holds the write token and no
signing secret; `promote` is its own job. `finalize` and `promote` run under
`!cancelled() && !failure()`, which tolerates a skipped needed job and stops on a
failed one, so a failed leg leaves a draft.

The gate is a full re-run rather than a reference to the commit's CI result,
because a tag can be pushed to any commit — including one whose PR checks never
ran.

**Two archives, and both are load-bearing.** The `.tar.gz` is what a person
downloads; the RAW binary is the one the bill of materials names, so it is what
`tumika update` and `scripts/install-daemon.sh` both fetch (ADR-0003), because
the updater replaces the running binary with an atomic rename and needs one
uncompressed file at a predictable URL. The BOM generator finds it by the prefix
`tumika_<daemon component version>_`, which is why the name template is part of
the contract. `scripts/verify-release-assets.sh` checks that contract on both
the snapshot build in CI and the real release — dropping the raw archive,
renaming a template, or losing a target are all one-line edits that leave the
build perfectly green.

**The release label and the component versions come from `release.yaml` at the
repo root, not from the tag.** The tag is `v<label>` and only names the release;
a tag that is not that string fails the gate. The label is CalVer, and each
component's version is semver; both are chosen by editing that file, and the
file is attached to every release as an asset. `scripts/release-label.sh` is the
one place the label's shape is written in shell — the same pattern as
`release.ValidateReleaseLabel`, with a test in `platform/release` failing when
the two drift. `scripts/release-component-version.sh <component>` does the same
for a component's semver. The goreleaser job exports `TUMIKA_RELEASE` (which
`-X main.release` reads) and `TUMIKA_DAEMON_VERSION` (which `-X main.version`,
every asset name and the snapshot version read), and the image job's `RELEASE`
and `VERSION` build args come from the same two scripts. A value derived from
the tag is no value at all: a label the daemon rejects leaves the build on the
`dev` default, which reads as "no release" and makes the recency rules and the
update watermark unreachable, and a component version taken from a CalVer tag
cannot be compared by the updater.

**Two checks in the gate job run before anything is built.**
`scripts/validate-release.sh --tag "$GITHUB_REF_NAME"` asserts the tag equals
`v<label>`. `scripts/check-release-monotonic.sh` asserts no component version is
lower than the one in the most recently published calendar release (drafts, the
in-flight tag and `edge-*` releases are skipped). An EQUAL component version is
accepted and means the component is carried over: it is not rebuilt, and the
BOM points at the earlier release's assets (`from_release`). The script reports
the components that differ as `changed=<list>`, the gate's `changed` output,
which gates the `goreleaser`, `desktop` and `image` jobs and reaches
`verify-release-assets.sh` as `TUMIKA_CHANGED_COMPONENTS`. A component named for
the first time counts as changed. A beta's component version carries a
`-beta.N` suffix, which semver orders below the stable version of the same core,
so the same comparison covers beta to stable.
It fails closed: an unreadable release list, or an unreadable asset list or
download for the previous release, fails the gate. Only a previous release whose
successfully fetched asset list has no `release.yaml` passes without a
comparison. The gate needs `contents: read` and nothing more.

**The label assertion fails rather than skips.** `verify-release-assets.sh`
requires `TUMIKA_RELEASE` on anything but a snapshot build, and asserts the
binary's own first line carries `(release <label>,`. Run it by hand with
`TUMIKA_RELEASE=$(scripts/release-label.sh) scripts/verify-release-assets.sh`.
A snapshot names itself (`<component version>-snapshot`) and is asserted against
the `dev` default instead, so `-X main.release` failing to reach the compiler is
caught on every CI run. `ci-build.yml` exports only `TUMIKA_DAEMON_VERSION` for
that reason.

The same script reads the component version from `release.yaml`, not from the
build, and asserts the asset names and the binary's reported version against it.
A stale or hand-set `TUMIKA_DAEMON_VERSION` would otherwise name the assets
consistently and pass. Reading `metadata.json` is NOT the same question:
goreleaser fills that in from the tag on a release whether or not the ldflags
reached the compiler, so its version is not a component version. Verified by
dropping `-X main.version` — the metadata stayed correct and the binary reported
`dev`, which `buildinfo.IsDev()` treats as "disable self-update entirely".

**The release is published as a DRAFT and promoted only after verification.**
A gate that runs after `release --clean` reports rather than prevents: the
broken release would already be downloadable. The `promote` job runs after every
component is attached.

**The desktop app is a component of the release.** The `desktop` matrix builds
`darwin_arm64` and `darwin_amd64` on one macOS runner, asserts
`scripts/desktop-version.sh --check` (the four committed copies of the desktop
component version — `tauri.conf.json`, `Cargo.toml`, `Cargo.lock`,
`package.json` — must equal `release.yaml`, so they move together in one
commit), renames Tauri's `Tumika.app.tar.gz` to
`tumika-desktop_<component version>_<platform>.app.tar.gz`, and checks the pair
with `scripts/verify-desktop-assets.sh`. `finalize` creates the draft when
`goreleaser` was skipped, uploads both archives and their `.sig` files, and
appends the archives' digests to `checksums.txt`; without those lines `bomgen`
skips the component. Four names begin `tumika`, told apart by the ending: the
daemon's raw binary, its `.tar.gz`, the app's `.app.tar.gz` and its `.sig`.
Signing is ad-hoc (`signingIdentity` `"-"`) unless all six `APPLE_*` secrets are
set, in which case the app is Developer ID signed and notarized; some but not
all fails the job. The updater signature comes from `TAURI_SIGNING_PRIVATE_KEY`
and `TAURI_SIGNING_PRIVATE_KEY_PASSWORD`.

The promote step passes `--prerelease` explicitly, derived from the label
(`-beta.` means prerelease). goreleaser's `prerelease: auto` cannot be templated
and reads the tag, so it is not the value the release ends up with; and the BOM
generator refuses a release whose GitHub prerelease flag disagrees with the
channel its tag names, so a beta promoted as a full release is one the site
never carries. The ghcr `:latest` tag is guarded separately, because `type=raw`
in `docker/metadata-action` is unconditional; it is enabled only for a stable
label.

**The site is published by a job of this workflow, not by its own trigger.**
`publish-pages.yml` listens for `release: published`, but the promote step
publishes with the default `GITHUB_TOKEN` and GitHub raises no workflow event
for anything that token does — so after a real release that trigger never fires.
The `pages` job dispatches the workflow on `main` (`gh workflow run`, which
`GITHUB_TOKEN` may do), after both the promote step and the image push. It does
not call the workflow with `uses:`, because the signing environment's secret
arrives empty in a called workflow. The dispatch returns once the run is queued,
so a failed site publish shows on the `publish-pages` run, not on the release
run. The weekly schedule and `workflow_dispatch` on `publish-pages.yml` remain
the recovery paths.

**A first install and a self-update share one trust chain.**
`scripts/install-daemon.sh` is served from `https://get.tumika.org`, never
attached to a release — a first-time user has no release to download it from. It
fetches a channel's signed bill of materials, verifies it against the release
public key embedded in the script, and reads the asset URL and SHA-256 out of
the verified document. `TUMIKA_CHANNEL` selects the channel (`stable`, the
default, `beta` or `edge`); `TUMIKA_VERSION` pins a release LABEL
(`2026.09.01`, `2026.09.01-beta.1`, `edge.417`; a leading `v` is accepted and
dropped) and replaces the channel lookup entirely. Neither names a component
version: the document does that. The workflow's asset check therefore asks only
for `release.yaml` and `checksums.txt` in the draft.

**Releases are cut from `main` only.** The gate asserts the tagged commit is an
ancestor of `main`: re-running the tests is not the same as knowing the commit
was reviewed, and anyone who can push a tag could otherwise point it at a commit
that merely compiles.

**Before the first publish, the repository needs these things nothing in the
workflows creates.**

- A DNS CNAME for `get.tumika.org` pointing at the GitHub Pages host.
- Settings, Pages, Source set to GitHub Actions. Any other source ignores the
  uploaded artifact and keeps serving whatever is there.
- A tag ruleset restricting who may create `v*.*.*` tags. The `release-signing`
  environment admits them, and a workflow at a tagged commit controls its own
  jobs, so no check inside the workflow can establish that a tag was cut from
  `main`: who may create the tag is the control.
- The environment `release-signing`, whose deployment-branch policy admits only
  `main` and `v*.*.*` tags, holding the secret `TUMIKA_RELEASE_SIGNING_KEY`: an
  ECDSA P-256 private key as PEM (SEC1 or PKCS#8), for example from `openssl ecparam -name prime256v1
  -genkey -noout`. Its public half must be the first entry of `releaseKeyPEMs`
  in `source/daemon/internal/platform/release/keys.go`, which is also the key
  embedded in `scripts/install-daemon.sh`; `installer_key_test.go` fails when
  the two differ. `tumika-bom` refuses a key that is not in the compiled-in list.
- A Tauri updater keypair (`pnpm tauri signer generate`): the public key is
  committed in `tauri.conf.json`, the private key's contents are the secret
  `TAURI_SIGNING_PRIVATE_KEY`, with `TAURI_SIGNING_PRIVATE_KEY_PASSWORD`.
  Losing the private key means shipping a new public key to every installed app
  (ADR-0010). The six `APPLE_*` secrets are optional, all or none.

**The site is assembled from four inputs and served from one host.**
`publish-pages.yml` runs `tumika-bom` (which reads the published releases and
writes every document and its detached `.sig`), then
`scripts/assemble-site.sh <bom-dir> <daemon-installer> <app-installer> <static-dir> <out-dir>`,
which adds both installers and `scripts/site/` and refuses a tree without an installer,
`CNAME`, `index.html`, a channel head, or a document's signature. It serves:

| Path | Content |
|---|---|
| `/channels/<channel>.json` (+ `.sig`) | the head release of `stable`, `beta` or `edge` |
| `/releases/<label>.json` (+ `.sig`) | one release's bill of materials |
| `/install-daemon.sh` | the daemon installer |
| `/install-app.sh` | the macOS app installer, served from the same host |
| `/`, `/CNAME` | the landing page and the custom domain record |

The BOMs are regenerated whole on every run, so re-running the workflow retries
a failed deploy. The generator reads each raw asset's SHA-256 from
`checksums.txt`, and skips a release lacking `release.yaml` or `checksums.txt`,
so goreleaser must keep uploading both. A skipped release fails the run unless
`-allow-skips` is passed. A component the generator does not know how to name
(`componentAssets` in `internal/bomgen`) is a skipped release too, so adding a
component adds a row there. A component with a signature rule publishes each
asset's `<name>.sig` text as the asset's optional `signature` field in the BOM,
and a release whose archive lacks its `.sig` is skipped.

**An edge build is cut by dispatching the `edge` workflow on `main`, naming the
thing to build:**

```sh
gh workflow run edge.yml --ref main -f ref=<branch, tag or commit>
```

Dispatching a workflow runs the workflow FILE from the dispatched ref, so
choosing a branch in the UI would run that branch's definition of every job —
including the permissions the jobs ask for. The ref is an input instead, and a
first `guard` job everything else needs fails the run when `github.ref` is not
`refs/heads/main`. The input is handed to `actions/checkout` and to `env:`, never
spliced into a `run:` script, where a ref named `$(…)` would execute.

The work splits in four, and the split is the point:

- `build` checks out `ref` with `persist-credentials: false` and holds
  `contents: read`. It runs the branch's `scripts/edge-version.sh` (labelling the
  build `edge.<n>` and suffixing every component version with `-edge.<n>`), tags
  the commit `edge-<run number>` **locally**, runs goreleaser with
  `--skip=validate,publish` and no token, runs
  `scripts/verify-release-assets.sh` — which executes the native binary, so it
  belongs in the job that has one — stages the release's assets into one
  directory and uploads them as an artifact.
- `desktop` does the same for the app under `contents: read` and no secret: it
  stamps the four component-version copies with the edge component version,
  builds with `createUpdaterArtifacts` off (the committed value is on) and packs
  the archive by hand.
- `sign` runs `main`'s checkout and pinned Tauri CLI with the updater key over
  the archives, downloaded as data, with `tauri signer sign`. It runs no branch
  code.
- `publish` runs `main`'s checkout with `contents: write` and `actions: write`.
  It treats the artifact strictly as data: it executes nothing out of it, runs no
  script from the built ref, appends the desktop archives' digests to
  `checksums.txt` (`scripts/verify-desktop-assets.sh --checksums`), and checks
  every downloaded file name against `scripts/edge-check-artifact.sh` before
  `gh release create` sees it. The tag is
  created by `--target <built commit>`, so no git credential and no branch code
  tags anything. Then it promotes the draft as a prerelease, prunes to the newest
  five edge releases (`scripts/edge-prune.sh`, which only touches tags spelled
  `edge-<digits>`), and dispatches `publish-pages.yml` on `main` rather than
  calling it.

Without that split, the branch's own scripts and goreleaser config run beside a
token that could `gh release upload --clobber` a published release's binary and
`checksums.txt` — which `publish-pages.yml` then signs.

`scripts/edge-check-artifact.sh <dir> <run-number>` is a closed allow-list, not
a filter: every entry must be a regular file directly in the directory (no
subdirectory, no symlink — `gh release create` would upload what a symlink points
at) and must be one of the four targets' raw binary and `.tar.gz`, the two desktop
`.app.tar.gz` archives with their `.sig`, `checksums.txt`, or `release.yaml`,
with all four daemon targets and both desktop platforms present, each component
under one version. The run number comes from `github.run_number`, so a build
cannot upload assets belonging to another run under this one's tag.
`goreleaser --skip=publish` leaves the raw binary in a per-target directory under
the name `tumika`, so the staging step reads the asset name it would have been
uploaded under out of `dist/artifacts.json`, and copies `release.yaml` explicitly
because `release.extra_files` is only read by the publish step that was skipped.

A cancelled run can leave a draft behind; the draft is never published and can be
deleted by hand. It leaves no tag: the tag comes with the release.

**What makes a published release trustworthy is the signature on its BOM.** The
BOM carries each asset's SHA-256, and the signature covers the exact bytes
served, so a checksum fetched from the same host as the binary is not what the
updater or the installer relies on. `checksums.txt` is an input to the
generator, not a trust root.
