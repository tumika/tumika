#!/usr/bin/env bash
# Checks that a goreleaser build produced the assets other things depend on.
#
# Three separate contracts run through these names, and none of them is checked
# by the compiler or by `goreleaser check`:
#
#   - scripts/install.sh downloads `tumika_<version>_<os>_<arch>` and verifies it
#     against checksums.txt
#   - `tumika update` fetches the same raw asset (ADR-0003)
#   - both would fail at the NEXT RELEASE rather than here, on somebody else's
#     machine, with no obvious cause
#
# Dropping the raw archive, renaming a template, or losing a target are all
# one-line edits to .goreleaser.yml that leave the build perfectly green.
#
# Usage: scripts/verify-release-assets.sh [dist-dir]
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

[[ -f "$ARTIFACTS" ]] || fail "no $ARTIFACTS — did goreleaser run?"
[[ -f "$CHECKSUMS" ]] || fail "no $CHECKSUMS; install.sh and the updater both verify against it"

# The version goreleaser used, taken from its own metadata rather than guessed.
VERSION=$(python3 -c "
import json
print(json.load(open('$DIST/metadata.json'))['version'])
")
[[ -n "$VERSION" ]] || fail "could not read the version from $DIST/metadata.json"

ok "version is $VERSION"

for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
  os="${target%%/*}"; arch="${target##*/}"
  asset="tumika_${VERSION}_${os}_${arch}"

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
  [[ -n "$found" ]] || fail "no raw asset '$asset'; install.sh and \`tumika update\` both fetch it by that name"
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

  # And it has to be verifiable, or install.sh refuses it.
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
# and the check passed.
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

grep -q "^tumika ${VERSION} " <<<"$first_line" \
  || fail "the binary reports '${first_line}', not version ${VERSION}; the ldflags did not take, and IsDev() would disable self-update"
ok "the binary itself reports ${VERSION}"

echo
echo "PASS"
