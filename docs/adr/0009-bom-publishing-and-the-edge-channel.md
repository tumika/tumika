---
status: accepted
date: 2026-09-20
---

# Bills of materials are generated, signed and published from GitHub Releases; edge is cut by dispatch

ADR-0006 defines the bill of materials and ADR-0007 the channels that select a head. This
records how the documents are produced and served, and how a build reaches the edge channel
without a release.

## Decisions

- **The generator reuses the daemon's own types.** `tumika-bom` lives in the daemon's module.
  `internal/bomgen` builds each document from `release.BOM` and round-trips it through
  `ParseBOM`, so the publisher cannot emit a shape the daemon does not parse. After signing and
  writing, `tumika-bom` reads the files back off disk and verifies every signature against the
  compiled-in key list; a tree that fails is never uploaded.

- **The whole site is regenerated from the Releases API on every run.** Nothing accumulates: a
  run reads the published releases and writes every release document and every channel head.
  Only published releases are read, so a head can only point at a release that exists, and a
  failed Pages deploy is recovered by re-running. `publish-pages.yml` runs weekly, on dispatch,
  and on `release: published`. `release.yml` calls it as a job and `edge.yml` dispatches it on
  `main`, because both publish with the default `GITHUB_TOKEN`, and GitHub raises no workflow
  event for anything that token does.

- **Signatures are detached and cover the exact bytes published.** Each `<document>.json` has a
  `<document>.json.sig` beside it. The bytes that are signed are the bytes that are written and
  served; nothing re-serialises them afterwards.

- **The signing key is an environment secret; the public keys are compiled in.**
  `TUMIKA_RELEASE_SIGNING_KEY` holds an ECDSA P-256 private key as PEM, either SEC1 or PKCS#8.
  It is a secret of the `release-signing` environment, whose deployment-branch policy admits
  only `main` and `v*.*.*` tags, and the signing job of `publish-pages.yml` selects that
  environment. No caller passes it, so no run from another branch can obtain it. The policy admits `v*.*.*`
  tags and cannot check that a tag was cut from `main`, and a workflow at a tagged commit
  controls its own jobs, so a check inside the workflow proves nothing: a tag ruleset limiting
  who may create those tags is what closes that path.
  `platform/release/keys.go` holds the list of public keys a daemon trusts. Rotation ships the
  new public key in a release signed by the old key; a daemon that applies it then accepts both.

- **Edge is dispatched on `main` and builds any ref.** `edge.yml` takes the branch, tag or
  commit to build as a `workflow_dispatch` input rather than being dispatched on it, because
  dispatching a workflow runs the workflow FILE from the chosen ref — so a run started on a
  branch would be that branch's definition of every job, including the permissions each one
  asks for. A first `guard` job everything else needs refuses a run whose `github.ref` is not
  `refs/heads/main`. The release is tagged `edge-<run number>`, a namespace that never matches
  `v*.*.*`, so it cannot start the calendar release workflow; the release label is `edge.<n>`
  and each component version carries an `-edge.<n>` suffix. The workflow keeps the newest five
  edge releases and deletes the rest with their tags; the prune considers only tags spelled
  `edge-<digits>` and never a `v*` tag. It dispatches `publish-pages.yml` on `main` rather than
  calling it, so a called workflow can never run the built ref's `tumika-bom` source with the
  signing key.

- **No branch-controlled code runs in a job holding a write token.** `edge.yml` splits in two.
  `build` checks out the named ref with `persist-credentials: false`, holds `contents: read`,
  runs that ref's scripts, goreleaser (`--skip=publish`, no token) and
  `verify-release-assets.sh`, tags only locally, and uploads the release's assets as an
  artifact. `publish` runs `main`'s checkout with `contents: write` and `actions: write`, treats
  the artifact strictly as data — it executes nothing out of it and runs no script from the
  built ref — validates every downloaded file name against `scripts/edge-check-artifact.sh`, and
  creates the release with `gh release create --target <built commit>`, which is what writes the
  tag. Undivided, the built branch's own `edge-version.sh` and `.goreleaser.yml` would run
  beside a token able to `gh release upload --clobber` over a published release's binary and
  `checksums.txt`, which `publish-pages.yml` then signs.

- **The installer verifies before it trusts.** `scripts/install-daemon.sh` is served from the
  site, not attached to a release. It verifies the BOM's signature with `openssl` against a
  public key embedded in the script, and only then reads the asset URL and SHA-256 out of the
  document. A test in `platform/release` fails when the embedded key and the first entry of
  `keys.go` differ.

## Considered alternatives

- **A separate `releases.` host for the documents.** Rejected: one GitHub Pages site is one
  host, so a second host is a second site, a second deploy and a second DNS record to keep in
  step with the first.
- **An unsigned `checksums.txt` as the trust root.** Rejected: it is served from the same
  place as the binary, so it proves the download was intact and nothing about who produced it.
  `checksums.txt` is an input to the generator, which reads each raw asset's SHA-256 from it;
  the signature on the BOM is the trust root.
- **Editing a static BOM by hand.** Rejected: a hand edit can name an asset that does not
  exist or a checksum that is wrong, and no build would notice. Generating from the published
  assets makes the document a function of what was actually released.

## Consequences

- A release the generator cannot publish is skipped whole. A skip fails the run unless
  `-allow-skips` is passed, so the run stays red instead of the release quietly vanishing from
  the site.
- A release whose GitHub prerelease flag disagrees with the channel its tag names is skipped,
  because nothing can tell which of the two is wrong.
- A release with no `release.yaml` or `checksums.txt` asset is skipped. goreleaser must keep
  uploading both.
- A cancelled edge run can leave a draft release behind. The draft is inert: the generator
  ignores drafts, and the prune ignores them too. It leaves no tag, because the tag is created
  with the release.
- The split bounds what an edge build can publish; it does not bound what a maintainer can.
  Anyone with repository write access can edit or replace the assets of any release directly,
  and can dispatch their own copy of `edge.yml` — a branch's workflow file is a workflow
  definition no check written in this one runs inside of. What is not done is attesting the
  provenance of a stable or beta release's assets before their digests are signed: the BOM
  generator reads whatever assets the release carries at the moment it runs. Closing that means
  signing at build time, or comparing against a build attestation, and neither is in place.
- Every deploy replaces the whole site, so a run that publishes a broken tree takes the
  channels down until the next successful run; `scripts/assemble-site.sh` refuses the tree
  shapes it can recognise as broken.
