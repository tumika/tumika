# Releasing

Tag `vX.Y.Z` and push it. `release.yml` then: re-runs the full gate (including
the systemd install harness) on that exact commit, runs `goreleaser release`,
and publishes a multi-arch image to `ghcr.io/tumika/tumika`.

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

It also runs the built binary and asserts it reports the release version.
Reading `metadata.json` is NOT the same question: goreleaser fills that in from
the tag whether or not the ldflags reached the compiler. Verified by dropping
`-X main.version` — the metadata stayed correct and the binary reported `dev`,
which `buildinfo.IsDev()` treats as "disable self-update entirely".

**The release is published as a DRAFT and promoted only after verification.**
A gate that runs after `release --clean` reports rather than prevents: the
broken release would already be downloadable. The promote step is the last thing
the job does.

`prerelease: auto` matters for the same reason: the tag glob `v*.*.*` matches
`v1.0.0-rc1`, and a full release at that tag would move `/releases/latest` — and
therefore the documented `curl … /releases/latest/download/install.sh` — onto an
RC. The ghcr `:latest` tag is guarded separately, because `type=raw` in
`docker/metadata-action` is unconditional.

**Releases are cut from `main` only.** The gate asserts the tagged commit is an
ancestor of `main`: re-running the tests is not the same as knowing the commit
was reviewed, and anyone who can push a tag could otherwise point it at a commit
that merely compiles.

**Before any public release:** sign `checksums.txt` with an ECDSA key from
Actions secrets and verify it in the updater. A checksum fetched from the same
host as the binary proves the download was not corrupted; it does not prove who
produced it.
