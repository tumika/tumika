#!/usr/bin/env bash
# Validates release.yaml: the release label, every component's version, and
# optionally that a tag names the release.
#
# A release's tag is v<label>. A tag that disagrees with the label would publish
# one release under another's name, so the workflow passes its tag here before
# building anything.
#
# Usage: scripts/validate-release.sh [--tag <tag>] [release.yaml]
set -euo pipefail

HERE="$(dirname "$0")"

fail() { echo "FAIL: $*" >&2; exit 1; }

TAG=""
FILE=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --tag)
      [[ $# -ge 2 ]] || fail "--tag needs a value"
      TAG="$2"
      shift 2
      ;;
    -*) fail "unknown option $1" ;;
    *)
      [[ -z "$FILE" ]] || fail "more than one file given"
      FILE="$1"
      shift
      ;;
  esac
done
FILE="${FILE:-$HERE/../release.yaml}"

label=$("$HERE/release-label.sh" "$FILE")

# Every entry of the top-level `components:` block, each validated by the same
# script that reads a single one.
names=$(awk '
  /^components:/ { inblock = 1; next }
  /^[^[:space:]#]/ { inblock = 0 }
  inblock && /^[[:space:]]+[^[:space:]#]/ {
    line = $0
    sub(/^[[:space:]]+/, "", line)
    n = index(line, ":")
    if (n > 0) print substr(line, 1, n - 1)
  }
' "$FILE")
[[ -n "$names" ]] || fail "no components under 'components:' in $FILE"

while IFS= read -r name; do
  "$HERE/release-component-version.sh" "$name" "$FILE" >/dev/null
done <<< "$names"

if [[ -n "$TAG" && "$TAG" != "v$label" ]]; then
  fail "tag '$TAG' is not v$label; the tag of the release $FILE names"
fi

echo "release $label ok"
