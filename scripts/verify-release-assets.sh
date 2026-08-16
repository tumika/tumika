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

fail() { echo "FAIL: $*" >&2; exit 1; }
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
  [[ -f "$found" ]] || fail "the raw asset '$asset' is registered at $found, which does not exist"

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

  # The human-facing archive, for the same four targets.
  [[ -f "$DIST/${asset}.tar.gz" ]] || fail "no archive '${asset}.tar.gz'"
done

ok "all four targets ship a raw binary and an archive"

# The ldflags have to have TAKEN, which only the binary can say.
#
# Asking metadata.json is not the same question: goreleaser fills that in from
# the tag whether or not the ldflags reached the compiler. Verified — dropping
# `-X main.version` left the metadata correct and the binary reporting "dev",
# and this check passed.
#
# It matters because "dev" is not merely a cosmetic default: buildinfo.IsDev()
# disables self-update entirely, so the release would ship a binary that can
# never update itself and nothing would report a problem.
host_os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$(uname -m)" in
  x86_64|amd64) host_arch=amd64 ;;
  aarch64|arm64) host_arch=arm64 ;;
  *) host_arch="" ;;
esac

if [[ -n "$host_arch" ]]; then
  native=$(python3 -c "
import json
a = json.load(open('$ARTIFACTS'))
print(next((x['path'] for x in a
            if x.get('type') == 'Binary'
            and x.get('goos') == '$host_os' and x.get('goarch') == '$host_arch'), ''))
")
  if [[ -n "$native" && -x "$native" ]]; then
    reported=$("$native" version | head -1)
    grep -q "^tumika ${VERSION} " <<<"$reported" \
      || fail "the binary reports '${reported}', not version ${VERSION}; the ldflags did not take, and IsDev() would disable self-update"
    ok "the binary itself reports ${VERSION}"
  else
    echo "  --: no native binary to run on ${host_os}/${host_arch}; skipping the ldflags check"
  fi
fi

echo
echo "PASS"
