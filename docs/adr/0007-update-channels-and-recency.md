---
status: accepted
date: 2026-09-20
---

# Channels are cumulative, publication order picks the head, and edge compares recency only

The daemon follows one channel, set by `update.channel`: stable, beta or edge. It has to
decide from a channel's head whether the running build should be replaced.

## Decisions

- **Channels are cumulative.** Stable receives stable releases, beta receives stable and
  beta, edge receives all three. A publisher writes each channel's document accordingly.

- **The head is the most recently published release the channel receives.** Publication
  order, not a label and not a version, picks it.

- **Stable and beta update only when the head is later AND its component version is
  semver-greater** than the running one, so a daemon there is never walked backwards by a
  republished older release.

- **Edge updates when the head is later, and compares nothing else.** A semver downgrade is
  allowed.

- **One rule serves `Check` and `Apply`**, so an update that is offered is never refused at
  install time.

## Considered alternatives

- **Pure semver on every channel.** Rejected: an edge build of a base version and a beta of
  the same base cannot both be "newer" than each other, and semver ranks a prerelease below
  the version it was cut from, so an edge daemon at `0.0.2-edge.147` would never move to
  `0.0.2`, and one at `0.0.2` would never take the next edge build. An edge release is an
  unvetted build of any branch, so its version carries no meaning; only its recency does.

## Consequences

- A daemon must know when its own release was published. It fetches its own release's BOM. A
  development build counts as older, so it is offered the head. Any failure other than a 404
  stops the check, because an unverifiable document says nothing about recency.
- **A 404 on the running build's own BOM falls back to a watermark**, the publication time the
  daemon recorded in `update_state` when it installed the build it is running. Edge decides on
  recency alone and a pruned BOM is indistinguishable from one a hostile host withholds, so
  without a floor that host could roll an edge daemon backwards by replaying a genuine, older,
  correctly signed channel head — no forged signature required. The watermark counts only while
  it belongs to the running build (its `to_version` is the running component version and its
  status is `pending` or `confirmed`). A daemon with no such watermark counts as older and is
  offered the head, which is what keeps a first install and a long-pruned edge build updatable.
- An edge daemon can be moved to a lower component version. That is deliberate.
- Publication time is written by the publisher into a signed document, so the ordering is as
  trustworthy as the signature.
