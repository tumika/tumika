---
status: accepted
date: 2026-09-20
---

# The release tag is the release label; component versions come from release.yaml

A release is a calendar label (ADR-0006) and each component inside it keeps its own semver. The
git tag that triggers the release workflow has to name one of those two things, and everything
the workflow builds has to be addressed by the other.

## Decisions

- **The tag is `v<label>` and only names the release.** `v2026.09.01`, `v2026.09.01-beta.1`.
  It is a zero-padded CalVer, which is not semver, so nothing that compares versions reads it.

- **Component versions come from `release.yaml`.** The file at the repository root holds the
  label and one semver per component. It is attached to every release as an asset, so a
  published release carries the record of what it was built from.

- **The gate enforces both halves before anything is built:**
  - the tag equals `v<label>` from `release.yaml`;
  - each component version is strictly greater than the one in the last published calendar
    release. A beta's component version carries a `-beta.N` suffix, which semver orders below
    the stable version of the same core, so one strict comparison covers beta to beta and beta
    to stable.
  - The comparison fails closed: if the release list or the previous release's assets cannot
    be read, the gate fails rather than passing without a comparison. A previous release that
    carries no `release.yaml` asset is the one case that passes, and it is decided from a
    successfully fetched asset list, never from a failed download.

- **Asset names, ldflags and image tags use the component version.** goreleaser reads it from
  `TUMIKA_DAEMON_VERSION` and hard-fails when it is unset. The image is tagged with the
  component version and with the label, and `:latest` only for a stable release.

- **The promote step sets `--prerelease` explicitly from the label.** goreleaser's
  `prerelease: auto` reads the tag and cannot be templated, so it is not the value the release
  ends up with; a label carrying `-beta.` is promoted as a prerelease.

- **`install.sh` reads the component version from the release's `release.yaml`.**
  `TUMIKA_VERSION` names a release tag, and the script downloads that release's `release.yaml`
  to learn which asset name to fetch.

## Considered alternatives

- **Derive the component version from the tag.** Rejected: one tag carries one version, which
  forces every component to share it and is the first alternative ADR-0006 rejected. It also
  makes the version a zero-padded label that the updater's semver comparison cannot parse.

## Consequences

- Cutting a release is an edit to `release.yaml` followed by a tag of `v<label>`. A tag that
  disagrees with the file is refused by the gate.
- A release whose component version does not advance is refused rather than published, because
  the update rules never move a stable or beta daemon to a lower or equal component version
  (ADR-0007). Every component listed in `release.yaml` must advance in each release; an
  unchanged component cannot be listed with its old version.
- `release.yaml` must ship with every release: the next release's gate and `install.sh` both
  download it.
- `metadata.json` from goreleaser reports the tag on a release, so it says nothing about the
  component version.
