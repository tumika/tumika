#!/usr/bin/env bash
# Fails when a component version in release.yaml does not advance past the one
# the most recently published release carries.
#
# The BOM reader never auto-downgrades: a stable or beta client takes the
# channel head only when its component version compares greater than the one it
# runs. A release that repeats or lowers a component version therefore publishes
# a head nobody moves onto — it installs cleanly, it is announced, and every
# daemon silently stays where it is. Nothing downstream can detect that, so the
# comparison happens here, before anything is built.
#
# The previous release is found through the Releases API rather than through git
# tags: a tag exists the moment it is pushed, while what clients can reach is
# what has been published. The in-flight release is still a draft at this point,
# and drafts are skipped for that reason.
#
# Beta releases count as published releases. A beta's component version carries
# a -beta.N prerelease suffix, which orders below the stable version of the same
# core (0.0.2-beta.1 < 0.0.2-beta.2 < 0.0.2), so the same strict comparison
# covers the whole beta-to-stable sequence and needs no separate rule.
#
# Requires `gh` authenticated for the repository (GH_TOKEN plus GH_REPO, or a
# checkout gh can infer the repository from).
#
# Usage: scripts/check-release-monotonic.sh [release.yaml]
set -euo pipefail

# Identifier comparison below is byte order, as semver 2.0.0 specifies; a
# locale's collation is not.
export LC_ALL=C

HERE="$(dirname "$0")"

fail() { echo "FAIL: $*" >&2; exit 1; }

[[ $# -le 1 ]] || fail "usage: $0 [release.yaml]"
FILE="${1:-$HERE/../release.yaml}"
[[ -f "$FILE" ]] || fail "no $FILE; it is the single source of the component versions"

# Prints -1, 0 or 1 as $1 sorts below, equal to, or above $2 in semver 2.0.0
# precedence order. Both arguments have already been validated as semver by
# release-component-version.sh.
semver_cmp() {
  # Build metadata is excluded from precedence.
  local a="${1%%+*}" b="${2%%+*}"
  local acore="${a%%-*}" bcore="${b%%-*}"
  local apre="" bpre=""
  if [[ "$a" == *-* ]]; then apre="${a#*-}"; fi
  if [[ "$b" == *-* ]]; then bpre="${b#*-}"; fi

  local -a acn bcn
  IFS=. read -r -a acn <<< "$acore"
  IFS=. read -r -a bcn <<< "$bcore"
  local i
  for i in 0 1 2; do
    if (( 10#${acn[$i]} > 10#${bcn[$i]} )); then echo 1; return; fi
    if (( 10#${acn[$i]} < 10#${bcn[$i]} )); then echo -1; return; fi
  done

  # A version without a prerelease outranks one with, and only then are the
  # identifiers compared.
  if [[ -z "$apre" && -z "$bpre" ]]; then echo 0; return; fi
  if [[ -z "$apre" ]]; then echo 1; return; fi
  if [[ -z "$bpre" ]]; then echo -1; return; fi

  local ai bi
  IFS=. read -r -a ai <<< "$apre"
  IFS=. read -r -a bi <<< "$bpre"
  local n=${#ai[@]}
  if (( ${#bi[@]} < n )); then n=${#bi[@]}; fi
  for (( i = 0; i < n; i++ )); do
    local x="${ai[$i]}" y="${bi[$i]}"
    if [[ "$x" == "$y" ]]; then continue; fi
    local xnum=0 ynum=0
    if [[ "$x" =~ ^[0-9]+$ ]]; then xnum=1; fi
    if [[ "$y" =~ ^[0-9]+$ ]]; then ynum=1; fi
    if (( xnum && ynum )); then
      if (( 10#$x > 10#$y )); then echo 1; else echo -1; fi
      return
    fi
    # A numeric identifier always sorts below an alphanumeric one.
    if (( xnum )); then echo -1; return; fi
    if (( ynum )); then echo 1; return; fi
    if [[ "$x" > "$y" ]]; then echo 1; else echo -1; fi
    return
  done
  # Equal as far as both go: the shorter set of identifiers sorts lower.
  if (( ${#ai[@]} > ${#bi[@]} )); then echo 1; return; fi
  if (( ${#ai[@]} < ${#bi[@]} )); then echo -1; return; fi
  echo 0
}

# The tag of the release being cut. validate-release.sh has already refused a
# tag that disagrees with the label, so this is the tag GitHub will carry — and
# it is excluded below, because comparing a release against itself would reject
# every re-run of a release whose draft was already published.
OWN_TAG="v$("$HERE/release-label.sh" "$FILE")"

# Published releases as "tag<TAB>isDraft<TAB>publishedAt". The filtering is done
# here rather than in the --jq expression so that one place decides what counts
# as a previous release. isPrerelease is deliberately not consulted: a beta is a
# published release like any other.
releases=$(gh release list --limit 100 \
  --json tagName,isDraft,publishedAt \
  --jq '.[] | [.tagName, .isDraft, .publishedAt] | @tsv') \
  || fail "could not list releases; refusing to pass the gate without knowing the previous release"

# A release tag, and nothing else. The edge workflow tags builds `edge-<n>`,
# which are published releases that must never be treated as the predecessor of
# a calendar release: their component versions carry an -edge suffix and move
# backwards by design.
TAG_PATTERN='^v[0-9]{4}\.[0-9]{2}\.[0-9]{2}(-beta\.[0-9]+)?$'

prev_tag=""
prev_published=""
while IFS=$'\t' read -r tag draft published; do
  [[ -n "$tag" ]] || continue
  [[ "$draft" != "true" ]] || continue
  [[ "$tag" != "$OWN_TAG" ]] || continue
  [[ "$tag" =~ $TAG_PATTERN ]] || continue
  # publishedAt is RFC 3339 in UTC, so byte order is chronological order.
  if [[ -z "$prev_published" || "$published" > "$prev_published" ]]; then
    prev_tag="$tag"
    prev_published="$published"
  fi
done <<< "$releases"

if [[ -z "$prev_tag" ]]; then
  echo "no published release to compare against; every component version in $FILE is a first"
  exit 0
fi

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# A release cut before release.yaml became an asset has nothing to compare
# against. That is decided from the asset list, never from a download failing:
# a transient API or network error looks the same as a missing asset, and
# treating it as one would pass the gate without a comparison. Any failure
# other than release.yaml being absent from a successfully fetched list fails.
assets=$(gh release view "$prev_tag" --json assets --jq '.assets[].name') \
  || fail "could not read the assets of $prev_tag; refusing to pass the gate without a comparison"

if ! grep -Fxq release.yaml <<< "$assets"; then
  echo "$prev_tag carries no release.yaml asset; no component version to compare against"
  exit 0
fi

gh release download "$prev_tag" --pattern release.yaml --dir "$tmp" >/dev/null \
  || fail "could not download release.yaml from $prev_tag"
[[ -f "$tmp/release.yaml" ]] || fail "release.yaml downloaded from $prev_tag is missing"

# Every entry of the top-level `components:` block of the new file, in the same
# shape validate-release.sh reads them.
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
  new=$("$HERE/release-component-version.sh" "$name" "$FILE")
  if ! old=$("$HERE/release-component-version.sh" "$name" "$tmp/release.yaml" 2>/dev/null); then
    echo "$name $new is new since $prev_tag"
    continue
  fi
  if [[ "$(semver_cmp "$new" "$old")" != "1" ]]; then
    fail "component '$name' is $new in $FILE and $old in $prev_tag; a released component version must compare greater than the last published one"
  fi
  echo "$name $old -> $new"
done <<< "$names"

echo "release.yaml advances every component version past $prev_tag"
