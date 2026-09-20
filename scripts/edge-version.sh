#!/usr/bin/env bash
# Prints the release label and the daemon component version an edge build is
# stamped with, and optionally writes the release.yaml that build publishes.
#
#   TUMIKA_DAEMON_VERSION=<release.yaml daemon version>-edge.<run number>
#   TUMIKA_RELEASE=edge.<run number>
#
# Both lines are KEY=VALUE, so the edge workflow appends the output to
# $GITHUB_ENV whole: a redirect keeps this script's exit status, whereas
# `echo "X=$(...)" >> "$GITHUB_ENV"` keeps echo's and would export an empty
# value from a failed run.
#
# The run number is what distinguishes one edge build from another. Two edge
# builds of the same commit are two different component versions, so the
# updater's comparison never sees one binary claiming to be another; the number
# also increases for every run of the workflow, which is what orders edge builds
# and what scripts/edge-prune.sh prunes by.
#
# Neither value's shape is written here. The label goes through
# scripts/release-label.sh and every component version through
# scripts/release-component-version.sh, which are the only two places in shell
# that spell those shapes and which the tests in
# source/daemon/internal/platform/release hold to the daemon's own patterns. A
# label the daemon rejects turns every update check on the built binary into an
# error, and a component version that is not semver is one the updater cannot
# compare at all.
#
# `--write <path>` writes the release.yaml the edge build publishes as an asset:
# the input file with every component version carrying the -edge.<n> suffix, and
# the top-level `release:` key left alone. That key names the CALENDAR release
# the commit was cut from — an edge build has no calendar release of its own,
# and the BOM generator reads the edge label from the tag rather than from this
# file. Suffixing the versions in the asset is what makes the file agree with
# the asset names goreleaser produces, which is what
# scripts/verify-release-assets.sh checks the build against.
#
# Usage: scripts/edge-version.sh [--write <path>] <run-number> [release.yaml]
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"

fail() { echo "FAIL: $*" >&2; exit 1; }

usage="usage: $0 [--write <path>] <run-number> [release.yaml]"

WRITE=""
args=()
while [[ $# -gt 0 ]]; do
  case "$1" in
    --write)
      [[ $# -ge 2 ]] || fail "--write takes a path"
      WRITE="$2"
      shift 2
      ;;
    -*) fail "unknown option '$1'; $usage" ;;
    # Collected rather than assigned in place: an empty argument is still an
    # argument, and taking the first non-empty one as the run number would read
    # the release.yaml path as it instead.
    *) args+=("$1"); shift ;;
  esac
done

[[ ${#args[@]} -ge 1 && ${#args[@]} -le 2 ]] || fail "$usage"
RUN="${args[0]}"
FILE="${args[1]:-}"

# Digits only, and checked before the number reaches a label or a version. A run
# number is what both of those are built out of, so anything else would be
# reported as a malformed label rather than as the bad argument it is.
[[ "$RUN" =~ ^[0-9]+$ ]] || fail "'$RUN' is not a run number"

FILE="${FILE:-$HERE/../release.yaml}"
[[ -f "$FILE" ]] || fail "no $FILE; it is the single source of the component versions"

LABEL="edge.$RUN"
SUFFIX="-edge.$RUN"

# The input file's own label, read only to fail early on a release.yaml the
# calendar release workflow would refuse as well. An edge build publishes the
# file unchanged in this respect.
"$HERE/release-label.sh" "$FILE" >/dev/null \
  || fail "$FILE does not name a release label; an edge build publishes it as an asset"

# Every entry of the top-level `components:` block, with the suffix appended to
# each version and everything else — comments, indentation, key order — left as
# it was. The block ends at the next line starting in column 1, the same parse
# scripts/release-component-version.sh and the BOM generator both do.
edge_yaml=$(awk -v suffix="$SUFFIX" '
  /^components:/ { print; inblock = 1; next }
  /^[^[:space:]#]/ { inblock = 0 }
  inblock && /^[[:space:]]+[^[:space:]#]/ {
    indent = $0
    sub(/[^[:space:]].*$/, "", indent)
    body = $0
    sub(/^[[:space:]]+/, "", body)
    comment = ""
    if (match(body, /[[:space:]]*#.*$/)) {
      comment = substr(body, RSTART)
      body = substr(body, 1, RSTART - 1)
    }
    n = index(body, ":")
    if (n == 0) { print; next }
    name = substr(body, 1, n - 1)
    value = substr(body, n + 1)
    sub(/^[[:space:]]+/, "", value)
    sub(/[[:space:]]+$/, "", value)
    gsub(/["\047]/, "", value)
    if (value == "") { print; next }
    printf "%s%s: %s%s%s\n", indent, name, value, suffix, comment
    next
  }
  { print }
' "$FILE")

# The names in the block, so every component version is validated rather than
# only the daemon's.
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

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# The two values are validated by reading them back through the same two scripts
# the release workflow reads release.yaml with, off a file that names the edge
# label. Checking them any other way would mean writing the label and semver
# shapes a second time here, which is exactly how the shell and the daemon come
# to disagree about what a label is.
printf '%s\n' "$edge_yaml" | sed "s|^release:.*|release: $LABEL|" > "$tmp/edge.yaml"

got_label=$("$HERE/release-label.sh" "$tmp/edge.yaml") \
  || fail "'$LABEL' is not a release label the daemon accepts"
[[ "$got_label" == "$LABEL" ]] \
  || fail "the edge release.yaml names release '$got_label', not '$LABEL'"

daemon_version=""
while IFS= read -r name; do
  [[ -n "$name" ]] || continue
  version=$("$HERE/release-component-version.sh" "$name" "$tmp/edge.yaml") \
    || fail "component '$name' with the $SUFFIX suffix is not valid semver"
  [[ "$version" == *"$SUFFIX" ]] \
    || fail "component '$name' is $version in the edge release.yaml, which does not carry $SUFFIX"
  if [[ "$name" == daemon ]]; then
    daemon_version="$version"
  fi
done <<< "$names"

[[ -n "$daemon_version" ]] || fail "no component 'daemon' under 'components:' in $FILE"

if [[ -n "$WRITE" ]]; then
  printf '%s\n' "$edge_yaml" > "$WRITE"
fi

printf 'TUMIKA_DAEMON_VERSION=%s\n' "$daemon_version"
printf 'TUMIKA_RELEASE=%s\n' "$LABEL"
