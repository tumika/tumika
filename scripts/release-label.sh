#!/usr/bin/env bash
# Prints the release label named by release.yaml, or fails.
#
# The label comes from a committed file rather than from the tag. A tag names a
# component version (semver), which is not a label the daemon accepts, and a
# binary stamped with a label the daemon rejects turns every update check into an
# error — while a binary stamped with no label at all reads as "no release" and
# makes the recency half of the channel rules inert. Reading the label from
# release.yaml is what lets a release be stamped with one regardless of how the
# tag is spelled.
#
# The pattern below is the only place the label's shape is written in shell. It
# is the same shape as releaseLabelPattern in
# source/daemon/internal/platform/release/bom.go, and the test in that package
# fails when the two disagree.
#
# Usage: scripts/release-label.sh [release.yaml]
set -euo pipefail

FILE="${1:-$(dirname "$0")/../release.yaml}"

fail() { echo "FAIL: $*" >&2; exit 1; }

[[ -f "$FILE" ]] || fail "no $FILE; it is the single source of the release label"

# `release:` is a top-level key, so it starts at column 1 — an indented one
# belongs to some other mapping and is not the label.
keys=$(grep -c '^release:' "$FILE" || true)
[[ "$keys" -ne 0 ]] || fail "no top-level 'release:' key in $FILE"
[[ "$keys" -eq 1 ]] || fail "$keys top-level 'release:' keys in $FILE; exactly one names the release label"

# Strip the key, an inline comment, trailing blanks, and any quoting (\042 and
# \047 are the double and single quote; a label contains neither).
label=$(sed -n 's/^release:[[:space:]]*//p' "$FILE" \
  | sed -e 's/[[:space:]]*#.*$//' -e 's/[[:space:]]*$//' \
  | tr -d '\042\047')

[[ -n "$label" ]] || fail "the 'release:' key in $FILE names no label"

LABEL_PATTERN='^([0-9]{4}\.[0-9]{2}\.[0-9]{2}(-beta\.[0-9]{1,6})?|edge\.[0-9]{1,10})$'
[[ "$label" =~ $LABEL_PATTERN ]] \
  || fail "'$label' in $FILE is not a release label the daemon accepts (YYYY.MM.NN[-beta.N] or edge.N)"

printf '%s\n' "$label"
