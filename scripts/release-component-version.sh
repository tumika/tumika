#!/usr/bin/env bash
# Prints one component's semver from the `components:` block of release.yaml, or
# fails.
#
# The block is a top-level mapping whose entries are indented `name: version`
# lines; it ends at the next line that starts at column 1. The parse is plain
# text (bash 3.2 and awk, no yq) so the release workflow needs no extra tool.
#
# The pattern below is the only place a component version's shape is written in
# shell. It is semver 2.0.0: three numeric parts without leading zeros, an
# optional prerelease (a beta component version is X.Y.Z-beta.N) and optional
# build metadata. The test in source/daemon/internal/platform/release fails when
# it disagrees with golang.org/x/mod/semver.
#
# Usage: scripts/release-component-version.sh <component> [release.yaml]
set -euo pipefail

fail() { echo "FAIL: $*" >&2; exit 1; }

[[ $# -ge 1 ]] || fail "usage: $0 <component> [release.yaml]"
COMPONENT="$1"
FILE="${2:-$(dirname "$0")/../release.yaml}"

[[ -f "$FILE" ]] || fail "no $FILE; it is the single source of the component versions"
[[ "$COMPONENT" =~ ^[A-Za-z0-9_-]+$ ]] || fail "'$COMPONENT' is not a component name"

# Entries of the top-level `components:` block, as "name<TAB>value" with any
# inline comment and quoting removed (\042 and \047 are the double and single
# quote; a version contains neither).
entries=$(awk '
  /^components:/ { inblock = 1; next }
  /^[^[:space:]#]/ { inblock = 0 }
  inblock && /^[[:space:]]+[^[:space:]#]/ {
    line = $0
    sub(/^[[:space:]]+/, "", line)
    sub(/[[:space:]]*#.*$/, "", line)
    n = index(line, ":")
    if (n == 0) next
    name = substr(line, 1, n - 1)
    value = substr(line, n + 1)
    sub(/^[[:space:]]+/, "", value)
    sub(/[[:space:]]+$/, "", value)
    gsub(/["\047]/, "", value)
    printf "%s\t%s\n", name, value
  }
' "$FILE")

count=$(printf '%s\n' "$entries" | grep -c "^${COMPONENT}	" || true)
[[ "$count" -ne 0 ]] || fail "no component '$COMPONENT' under 'components:' in $FILE"
[[ "$count" -eq 1 ]] || fail "$count entries for component '$COMPONENT' in $FILE; exactly one names its version"

version=$(printf '%s\n' "$entries" | grep "^${COMPONENT}	" | cut -f2)
[[ -n "$version" ]] || fail "component '$COMPONENT' in $FILE names no version"

NUM='(0|[1-9][0-9]*)'
PRE_ID='(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)'
BUILD_ID='[0-9A-Za-z-]+'
VERSION_PATTERN="^${NUM}\\.${NUM}\\.${NUM}(-${PRE_ID}(\\.${PRE_ID})*)?(\\+${BUILD_ID}(\\.${BUILD_ID})*)?\$"
[[ "$version" =~ $VERSION_PATTERN ]] \
  || fail "'$version' for component '$COMPONENT' in $FILE is not valid semver (X.Y.Z[-prerelease])"

printf '%s\n' "$version"
