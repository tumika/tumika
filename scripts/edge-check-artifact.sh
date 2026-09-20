#!/usr/bin/env bash
# Checks that a downloaded edge build artifact contains exactly the files an
# edge release publishes, and nothing else.
#
# The edge workflow splits in two so that no branch-controlled code ever runs in
# a job holding a write token: the build job checks out the branch and holds
# `contents: read`, and the publish job runs main's workflow definition with
# `contents: write`. The only thing that crosses between them is this artifact,
# and the publish job treats it strictly as DATA — it executes nothing out of it.
#
# That makes the file NAMES the whole attack surface. They reach
# `gh release create` as arguments, they become the asset names every daemon and
# scripts/install-daemon.sh resolve out of a bill of materials, and they are
# produced by the branch's own release.yaml and .goreleaser.yml. A branch that
# renames an asset publishes something under a name nobody audited; a branch that
# adds one publishes a file the release was never meant to carry.
#
# So the set is closed rather than filtered. Every entry must be a regular file
# directly in the directory, and must be one of:
#
#   tumika_<version>-edge.<run>_<os>_<arch>          the raw binary, per target
#   tumika_<version>-edge.<run>_<os>_<arch>.tar.gz   the archive, per target
#   checksums.txt                                    what a download is verified against
#   release.yaml                                     what the BOM generator reads
#
# The run number is supplied by the caller from github.run_number, never read out
# of the artifact: it is the one component of an asset name the branch does not
# choose, and anchoring the pattern on it is what stops a build from uploading
# assets belonging to another run. All four targets must be present in both
# forms, under ONE version — a mixed set would publish a release whose assets
# describe two different builds.
#
# A symlink is refused rather than followed: `gh release create` would upload
# whatever it points at, under a name from this allow-list.
#
# Usage: scripts/edge-check-artifact.sh <dir> <run-number>
set -euo pipefail

fail() { echo "FAIL: $*" >&2; exit 1; }
ok()   { echo "  ok: $*"; }

usage="usage: $0 <dir> <run-number>"

[[ $# -eq 2 ]] || fail "$usage"
DIR="$1"
RUN="$2"

[[ -d "$DIR" ]] || fail "no directory '$DIR'; it is where the build job's artifact was downloaded"
# Digits only, and checked before it is spliced into the pattern below: a run
# number carrying anything else would widen the allow-list rather than anchor it.
[[ "$RUN" =~ ^[0-9]+$ ]] || fail "'$RUN' is not a run number"

# The asset name shape. The prerelease segment is optional and excludes `-` so
# that the `-edge.<run>` suffix is the last one: a daemon component version may
# itself be a prerelease (0.1.0-beta.1), and the edge build suffixes that.
ASSET_PATTERN="^tumika_([0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?-edge\.$RUN)_(linux|darwin)_(amd64|arm64)(\.tar\.gz)?$"

# A nested entry of any kind. Reported before the names, because a directory in
# the artifact means the upload staged something other than the release assets
# and the name checks below would describe only half of what is wrong.
nested=$(find "$DIR" -mindepth 2 -print)
[[ -z "$nested" ]] || fail "the artifact contains nested entries, which an edge release never carries:"$'\n'"$nested"

versions=""
have_checksums=""
have_release_yaml=""
names=""

while IFS= read -r path; do
  [[ -n "$path" ]] || continue
  name="${path##*/}"
  [[ ! -L "$path" ]] || fail "'$name' is a symlink; \`gh release create\` would upload whatever it points at"
  [[ -f "$path" ]] || fail "'$name' is not a regular file"

  case "$name" in
    checksums.txt) have_checksums=1 ;;
    release.yaml)  have_release_yaml=1 ;;
    *)
      [[ "$name" =~ $ASSET_PATTERN ]] \
        || fail "'$name' is not an edge-$RUN release asset; the artifact carries the release's assets and nothing else"
      versions+="${BASH_REMATCH[1]}"$'\n'
      ;;
  esac
  names+="$name"$'\n'
done < <(find "$DIR" -mindepth 1 -maxdepth 1 -print)

[[ -n "$have_checksums" ]] \
  || fail "no checksums.txt; it is what anyone checking a download compares against, and the BOM generator reads each raw asset's SHA-256 from it"
[[ -n "$have_release_yaml" ]] \
  || fail "no release.yaml; the BOM generator skips a release without one, which leaves the build published and invisible to every edge daemon"

distinct=$(printf '%s' "$versions" | sort -u)
[[ -n "$distinct" ]] || fail "the artifact carries no release assets at all"
[[ $(printf '%s\n' "$distinct" | wc -l) -eq 1 ]] \
  || fail "the artifact names more than one component version, so its assets describe two builds:"$'\n'"$distinct"
VERSION="$distinct"

# Every target, in both forms. An asset lost between goreleaser and the upload
# would otherwise publish a release that some machines have no binary in.
for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
  os="${target%%/*}"; arch="${target##*/}"
  for want in "tumika_${VERSION}_${os}_${arch}" "tumika_${VERSION}_${os}_${arch}.tar.gz"; do
    grep -qxF "$want" <<<"$names" || fail "no '$want' in the artifact"
  done
done

ok "edge-$RUN: four targets at $VERSION, each a raw binary and an archive"
ok "checksums.txt and release.yaml are both present"
ok "nothing else is"

echo
echo "PASS"
