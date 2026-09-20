---
about: the release label stamped into a binary must satisfy the daemon's own validator, goreleaser cannot source it, and a missing own bill of materials is backed by a stored watermark; what breaks if any of these change
saw:
  - source/daemon/internal/platform/release/bom.go
  - source/daemon/internal/service/update.go
  - source/daemon/.goreleaser.yml
  - scripts/release-label.sh
  - scripts/verify-release-assets.sh
  - release.yaml
  - source/daemon/internal/repository/migrations/0002_update_state_published_at.sql
---
- A label stamped with `-X main.release` that fails `ValidateReleaseLabel` (bom.go) makes `publishedAt` in update.go return an error, and every `Check` with it: an invalid label is worse than the `dev` default, which just dates the build at the zero time. scripts/release-label.sh holds the only shell copy of the label pattern, and a test in platform/release fails when it drifts from `releaseLabelPattern`.
- goreleaser's ldflags templates cannot run a command, so the label reaches the build only through the `TUMIKA_RELEASE` environment variable (`envOrDefault` in .goreleaser.yml). Every invoker must export it, and verify-release-assets.sh fails a non-snapshot build that has none rather than skipping, because a build without it silently ships `release dev` and leaves the recency rule inert.
- Edge decides on recency alone, so a host that 404s the running build's own `/releases/<label>.json` while replaying an older signed channel head would roll the daemon back. `Apply` therefore stores the installed head's publication time in `update_state.to_published_at`, and `watermark` in update.go falls back to it only on `ErrNoRelease`, and only while the row's `to_version` is the running component version and its status is pending or confirmed. A daemon with no watermark still takes the head, so a pruned edge release stays survivable.
- `Check` never writes: a cleanly read own BOM wins and does not raise the watermark, because Check is called on a timer and must stay a read of the update row.
