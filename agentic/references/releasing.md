# Releasing

Edit `release.yaml` (the label and each component version), merge it to `main`,
then tag `v<label>` and push the tag. `release.yml` then: re-runs the full gate
(including the systemd install harness) on that exact commit, runs `goreleaser
release`, and publishes a multi-arch image to `ghcr.io/tumika/tumika`. ADR-0008
records why the tag names the release and nothing else.

The gate is a full re-run rather than a reference to the commit's CI result,
because a tag can be pushed to any commit — including one whose PR checks never
ran.

**Two archives, and both are load-bearing.** The `.tar.gz` is what a person
downloads; the RAW binary is what `tumika update` fetches and what
`scripts/install.sh` downloads (ADR-0003), because the updater replaces the
running binary with an atomic rename and needs one uncompressed file at a
predictable URL. `scripts/verify-release-assets.sh` checks that contract on both
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
`v<label>`. `scripts/check-release-monotonic.sh` asserts each component version
is strictly greater than the one in the most recently published calendar
release (drafts, the in-flight tag and `edge-*` releases are skipped). A beta's
component version carries a `-beta.N` suffix, which semver orders below the
stable version of the same core, so the same comparison covers beta to stable.
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
broken release would already be downloadable. The promote step is the last thing
the job does.

The promote step passes `--prerelease` explicitly, derived from the label
(`-beta.` means prerelease). goreleaser's `prerelease: auto` cannot be templated
and reads the tag, so it is not the value the release ends up with; a beta
promoted as a full release would move `/releases/latest` — and therefore the
documented `curl … /releases/latest/download/install.sh` — onto a beta. The
ghcr `:latest` tag is guarded separately, because `type=raw` in
`docker/metadata-action` is unconditional; it is enabled only for a stable
label.

**`install.sh` is addressed by release tag.** `TUMIKA_VERSION` names a release
tag (`v2026.09.01`, with or without the `v`), not a component version. The script
downloads that release's `release.yaml` asset to learn the daemon component
version and so the asset name, which is why the workflow's install-command step
requires `install.sh`, `release.yaml` and `checksums.txt` in the draft.

**Releases are cut from `main` only.** The gate asserts the tagged commit is an
ancestor of `main`: re-running the tests is not the same as knowing the commit
was reviewed, and anyone who can push a tag could otherwise point it at a commit
that merely compiles.

**Before any public release:** sign `checksums.txt` with an ECDSA key from
Actions secrets and verify it in the updater. A checksum fetched from the same
host as the binary proves the download was not corrupted; it does not prove who
produced it.
