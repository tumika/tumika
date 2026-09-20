---
status: accepted
date: 2026-09-20
---

# Releases are CalVer labels no client compares; components keep semver; a signed bill of materials maps one to the other

tumika ships more than one thing: the daemon and the desktop app. Each changes on its own
schedule, and the app must run the build that pairs with the daemon it talks to. The daemon
also has to decide, from a static host and without trusting the network, what to install.

## Decisions

- **A release is a CalVer label** of the form `YYYY.MM.NN`, zero-padded, with a `-beta.N`
  suffix for a beta and `edge.<run number>` for an edge build. The label is for people. Zero-padded CalVer
  is not valid semver, and that is acceptable because **no client ever compares a release
  label**; ordering comes from publication time (ADR-0007). A label that reaches a URL is
  first checked against a strict pattern.

- **A component keeps its own semver.** The daemon's component version is what the updater
  compares, what `tumika version` prints as its second field, and what names the archive.

- **A signed bill of materials (BOM) maps a release to component versions.** It is a JSON
  document listing, per component, the version, the download URL and SHA-256 for each
  `<os>_<arch>`, and `from_release` when an unchanged component is carried over from an
  earlier release rather than rebuilt. Bytes are never republished under one component
  version. The BOM is signed with a detached ECDSA P-256 signature and verified against a
  compiled-in list of public keys, so a key can be rotated. The BOM's SHA-256 replaces reliance
  on an unsigned checksums file.

## Considered alternatives

- **One CalVer for everything.** Rejected: a component that did not change would still get a
  new version, so the daemon binary would be republished under a new number with identical
  bytes, and a client comparing versions could not tell a real change from a relabel. It also
  gives up semver's meaning for the only thing a daemon compares.
- **CalVer manifests as wardnet uses them.** The channel model here is modelled on
  `wardnet/wardnet`, but its CalVer manifests are not adopted as they are: the daemon
  compares a component semver and must never compare a release label, so the label and the
  component versions are kept apart and joined by the signed BOM.

## Consequences

- Nothing in the daemon parses or orders a release label; `Later` compares publication times
  and `Newer` compares component semver.
- A release with no BOM published is a release the daemon cannot resolve: a missing document
  is "no release", and a missing signature is a refusal.
- Rotating the signing key takes two releases: one signed by the old key that adds the new
  one, then one signed by the new key. Removing a key ends its authority.
