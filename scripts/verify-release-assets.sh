#!/usr/bin/env bash
# Checks that a goreleaser build produced the assets other things depend on.
#
# Three separate contracts run through these names, and none of them is checked
# by the compiler or by `goreleaser check`:
#
#   - the BOM generator finds a release's assets by the prefix
#     `tumika_<daemon component version>_`, and publishes each one's URL and
#     SHA-256 to every daemon and to scripts/install-daemon.sh
#   - `tumika update` fetches the raw asset the BOM names (ADR-0003)
#   - both would fail at the NEXT RELEASE rather than here, on somebody else's
#     machine, with no obvious cause
#
# Dropping the raw archive, renaming a template, or losing a target are all
# one-line edits to .goreleaser.yml that leave the build perfectly green.
#
# The version every assertion below is made against comes from release.yaml, the
# single source of the component versions — the same place .goreleaser.yml takes
# it from, through TUMIKA_DAEMON_VERSION. Asking the build what version it used
# would make the check agree with itself: a build carrying a stale or hand-set
# TUMIKA_DAEMON_VERSION would name its assets consistently and pass, and the
# release would ship a daemon whose component version is not the one the BOM and
# the tag describe.
#
# A release that carries the daemon over unchanged builds no daemon assets, and
# demanding them would fail it. TUMIKA_CHANGED_COMPONENTS is the gate's
# `changed=` list (scripts/check-release-monotonic.sh): when it is set and does
# not name the daemon, every assertion below is skipped. Leaving it unset means
# the daemon changed, which is what every caller that builds the daemon
# unconditionally — the snapshot build in ci-build.yml, the edge build — relies
# on. An absent list is not an empty one.
#
# Usage: TUMIKA_CHANGED_COMPONENTS=daemon,desktop scripts/verify-release-assets.sh [dist-dir]
set -euo pipefail

DIST="${1:-dist}"
ARTIFACTS="$DIST/artifacts.json"
CHECKSUMS="$DIST/checksums.txt"

# goreleaser records artifact paths relative to ITS working directory, always
# as dist/…. Reading metadata from one tree and files from another failed at the
# file-exists check with a message blaming goreleaser, so paths are rebased onto
# $DIST here.
rebase() {
  local path="$1"
  if [[ "$DIST" != "dist" && "$path" == dist/* ]]; then
    printf '%s/%s\n' "$DIST" "${path#dist/}"
  else
    printf '%s\n' "$path"
  fi
}

fail() { echo "FAIL: $*" >&2; exit 1; }
NATIVE=""
ok()   { echo "  ok: $*"; }

# Whether the daemon is one of the components this release builds. The list is
# validated rather than merely searched: a garbled or empty value would
# otherwise skip every assertion in this script and still print PASS.
if [[ -n "${TUMIKA_CHANGED_COMPONENTS+set}" ]]; then
  CHANGED="$TUMIKA_CHANGED_COMPONENTS"
  [[ -n "$CHANGED" ]] \
    || fail "TUMIKA_CHANGED_COMPONENTS names no component; a release that rebuilds nothing publishes nothing to verify. Unset it to assert the daemon's assets"
  [[ "$CHANGED" =~ ^[A-Za-z0-9_-]+(,[A-Za-z0-9_-]+)*$ ]] \
    || fail "TUMIKA_CHANGED_COMPONENTS='$CHANGED' is not a comma-separated list of component names"
  case ",$CHANGED," in
    *,daemon,*) ;;
    *)
      echo "TUMIKA_CHANGED_COMPONENTS=$CHANGED does not name the daemon: it is carried over from an earlier release, which is the release that carries its assets"
      echo
      echo "PASS"
      exit 0
      ;;
  esac
fi

[[ -f "$ARTIFACTS" ]] || fail "no $ARTIFACTS — did goreleaser run?"
[[ -f "$CHECKSUMS" ]] || fail "no $CHECKSUMS; it is what anyone checking a download compares against"

# The daemon component's version, from release.yaml — not from the build.
VERSION=$("$(dirname "$0")/release-component-version.sh" daemon) \
  || fail "could not read the daemon component version from release.yaml"

# Whether this is a snapshot, decided from metadata.json's version rather than
# from the asset names the checks below are about to assert. metadata.json's
# version is the TAG on a release and `<component version>-snapshot` on a
# snapshot, because .goreleaser.yml's snapshot.version_template says so and
# goreleaser applies that template in no other mode; a release tag is a CalVer
# label and cannot end in "-snapshot". Reading the suffix off the archive names
# instead would let a snapshot build slip through a release job unremarked —
# the names would define the very thing they are being checked against.
META_VERSION=$(python3 -c "
import json
print(json.load(open('$DIST/metadata.json'))['version'])
")
[[ -n "$META_VERSION" ]] || fail "could not read the version from $DIST/metadata.json"

if [[ "$META_VERSION" == *-snapshot ]]; then
  SNAPSHOT=1
  ASSET_VERSION="${VERSION}-snapshot"
else
  SNAPSHOT=""
  ASSET_VERSION="$VERSION"
fi

ok "release.yaml names daemon $VERSION; assets are expected at $ASSET_VERSION"

for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
  os="${target%%/*}"; arch="${target##*/}"
  asset="tumika_${ASSET_VERSION}_${os}_${arch}"

  # The RAW binary, which is what gets downloaded and executed directly.
  # goreleaser leaves it in a per-target directory rather than at the dist root
  # and uploads it under this name, so the artifact metadata is what to check —
  # looking for a file at $DIST/$asset finds nothing and would fail for the
  # wrong reason.
  found=$(python3 -c "
import json
a = json.load(open('$ARTIFACTS'))
print(next((x['path'] for x in a
            if x.get('type') == 'Binary' and x.get('name') == '$asset'), ''))
")
  [[ -n "$found" ]] || fail "no raw asset '$asset'; the BOM generator finds it by that name and \`tumika update\` fetches it, and release.yaml is what names it"
  found=$(rebase "$found")
  [[ -f "$found" ]] || fail "the raw asset '$asset' is registered at $found, which does not exist"

  # Remembered so the ldflags check below can run the binary it already
  # located, rather than querying for a "native" one that may not be found.
  if [[ "$os" == "$(uname -s | tr '[:upper:]' '[:lower:]')" ]]; then
    case "$(uname -m)" in
      x86_64|amd64) [[ "$arch" == amd64 ]] && NATIVE="$found" ;;
      aarch64|arm64) [[ "$arch" == arm64 ]] && NATIVE="$found" ;;
    esac
  fi

  # And it has to be checksummed, so a download can be verified by hand.
  want=$(awk -v a="$asset" '$2 == a { print $1 }' "$CHECKSUMS")
  [[ -n "$want" ]] || fail "no checksum for '$asset' in checksums.txt"

  if command -v sha256sum >/dev/null 2>&1; then
    got=$(sha256sum "$found" | awk '{print $1}')
  else
    got=$(shasum -a 256 "$found" | awk '{print $1}')
  fi
  [[ "$got" == "$want" ]] || fail "checksum mismatch for '$asset': built $got, checksums.txt says $want"

  ok "$asset — present and matches its checksum"

  # The human-facing archive, for the same four targets — and verified, not
  # merely present: checksums.txt is what anyone checking a download compares
  # against, so an archive missing from it is an archive nobody can verify.
  [[ -f "$DIST/${asset}.tar.gz" ]] || fail "no archive '${asset}.tar.gz'"
  awk -v a="${asset}.tar.gz" '$2 == a { found = 1 } END { exit !found }' "$CHECKSUMS" \
    || fail "no checksum for '${asset}.tar.gz' in checksums.txt"
done

ok "all four targets ship a raw binary and an archive"

# The ldflags have to have TAKEN, which only the binary can say.
#
# Asking metadata.json is not the same question: goreleaser fills that in from
# the tag whether or not the ldflags reached the compiler. Verified — dropping
# `-X main.version` left the metadata correct and the binary reporting "dev",
# and the check passed. Nor is the asset name the same question: the name comes
# from the archive template, the stamp from the ldflags, and either can be
# edited without the other.
#
# It matters beyond cosmetics: buildinfo.IsDev() disables self-update entirely,
# so the release would ship a binary that can never update itself and nothing
# would report a problem.
#
# There is deliberately NO skip path. An earlier version bailed out quietly when
# it could not identify a native binary — on an unrecognised uname, or if the
# artifacts query returned nothing — and still printed PASS, which turns the one
# assertion this script exists for into a no-op the day goreleaser renames a
# metadata field. The loop above already located and checksummed a binary for
# this host, so "there isn't one" means the query is broken, not that the
# situation is benign.
[[ -n "$NATIVE" ]] \
  || fail "no binary for this host among the verified assets; the artifact query is broken, and the ldflags check would otherwise be skipped"
[[ -x "$NATIVE" ]] || fail "$NATIVE is not executable"

# Captured whole, THEN split. Piping into `head -1` closes the pipe after the
# first of the two lines `tumika version` prints, which under `set -o pipefail`
# can propagate a SIGPIPE exit and kill this script with no message — on the
# release job, after the release exists.
reported=$("$NATIVE" version)
first_line=${reported%%$'\n'*}

grep -q "^tumika ${ASSET_VERSION} " <<<"$first_line" \
  || fail "the binary reports '${first_line}', not version ${ASSET_VERSION}; release.yaml names the daemon ${VERSION}, so either the ldflags did not take or the build used a different TUMIKA_DAEMON_VERSION"
ok "the binary itself reports ${ASSET_VERSION}"

# The release LABEL, which `-X main.release` carries and which nothing above
# would notice the loss of: metadata.json has no such field, and a binary
# stamped with the "dev" label builds, runs and reports a correct version.
#
# It is the label the updater compares recency against, so without it a stable
# or beta daemon degrades to a semver-only comparison and an edge daemon accepts
# any head — a channel rule that silently stops applying.
#
# There is no skip path here either, for the same reason as above. A release
# build takes its label from release.yaml, so on one "TUMIKA_RELEASE is unset"
# means the build was never stamped — exactly the case this assertion exists to
# catch — rather than a situation in which the assertion cannot be made.
#
# A snapshot build is the one that legitimately carries no label: it is never
# published, and it says so in metadata.json. It is still asserted, against the
# "dev" default, so a `-X main.release` that stops reaching the compiler is
# caught on every CI run rather than at the next release.
if [[ -n "${TUMIKA_RELEASE:-}" ]]; then
  want_release="$TUMIKA_RELEASE"
else
  [[ -n "$SNAPSHOT" ]] \
    || fail "TUMIKA_RELEASE is unset, so this build carries no release label; export it first: TUMIKA_RELEASE=\$(scripts/release-label.sh)"
  want_release=dev
fi

grep -qF "(release ${want_release}," <<<"$first_line" \
  || fail "the binary reports '${first_line}', not release ${want_release}; -X main.release did not take, and the recency half of the update rules would be inert"
ok "the binary itself reports release ${want_release}"

echo
echo "PASS"
