#!/usr/bin/env bash
# Prints the edge releases that have fallen out of the newest few, and deletes
# them with their tags when asked to.
#
# Edge builds are cut on demand from any branch, so they accumulate without
# limit; every one of them carries four raw binaries and an archive per target.
# Keeping the newest few is what stops the repository's release list from being
# mostly edge builds nobody is running, and a deleted edge release is a 404 the
# daemon reads as "older than what I run" rather than as an error.
#
# The release list arrives on stdin as "tag<TAB>isDraft[<TAB>createdAt]" lines,
# which is what
#
#   gh release list --json tagName,isDraft,createdAt \
#     --jq '.[] | [.tagName, .isDraft, .createdAt] | @tsv'
#
# emits. Taking the list rather than fetching it is what lets the tests decide
# exactly which releases exist. The timestamp is accepted so that output can be
# piped in unchanged; it is not what orders the builds.
#
# Three rules make this safe to run in a job that holds contents: write.
#
#   - ONLY a tag spelled exactly edge-<digits> is ever considered. A calendar
#     release tag, a branch-shaped tag, anything at all outside that pattern is
#     not printed and not deleted. This script's whole risk is that it deletes
#     the wrong thing, and the pattern is the one line preventing it.
#   - A DRAFT is ignored entirely: neither deleted nor counted towards the
#     number kept. A draft is an edge build another run is still assembling, and
#     two edge runs overlapping is the ordinary case rather than an accident.
#   - Order is the RUN NUMBER, not the creation time. Run numbers increase for
#     every run of the workflow, so two overlapping runs that publish out of
#     order still prune to the same set — where timestamps would make the later
#     build the one dropped.
#
# A delete that fails is reported and does not fail the run: under overlap the
# other run may have deleted the same release a moment earlier, and the next
# edge build prunes whatever is still there.
#
# Usage: scripts/edge-prune.sh [--keep <n>] [--delete] < releases.tsv
set -euo pipefail

fail() { echo "FAIL: $*" >&2; exit 1; }

usage="usage: $0 [--keep <n>] [--delete] < releases.tsv"

KEEP=5
DELETE=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --keep)
      [[ $# -ge 2 ]] || fail "--keep takes a number"
      KEEP="$2"
      shift 2
      ;;
    --delete) DELETE=1; shift ;;
    *) fail "unknown argument '$1'; $usage" ;;
  esac
done

[[ "$KEEP" =~ ^[0-9]+$ ]] || fail "'$KEEP' is not a number of releases to keep"

# An edge tag, and nothing else.
EDGE_PATTERN='^edge-[0-9]+$'

edges=""
# The `|| [[ -n "$tag" ]]` keeps the last line when the input does not end in a
# newline: read reports failure there having already filled the variables, and
# dropping it would silently spare one release from every prune.
while IFS=$'\t' read -r tag draft _ || [[ -n "$tag" ]]; do
  [[ -n "$tag" ]] || continue
  # An absent isDraft column would make every draft look published, so it is
  # required rather than defaulted.
  [[ -n "$draft" ]] || fail "'$tag' has no isDraft column; $usage"
  [[ "$draft" != "true" ]] || continue
  [[ "$tag" =~ $EDGE_PATTERN ]] || continue
  edges+="${tag#edge-}"$'\t'"$tag"$'\n'
done

# Newest first by run number, then everything past the ones kept. The sort is
# numeric: edge-10 is newer than edge-9, which a byte comparison would get
# backwards and would quietly delete the newest build in every tenth run.
doomed=""
if [[ -n "$edges" ]]; then
  doomed=$(printf '%s' "$edges" | sort -t$'\t' -k1,1nr | tail -n "+$((KEEP + 1))" | cut -f2)
fi

if [[ -z "$doomed" ]]; then
  exit 0
fi

while IFS= read -r tag; do
  [[ -n "$tag" ]] || continue
  # Asserted a second time, on the value about to be handed to `gh release
  # delete`. The cost is one comparison; what it buys is that no edit to the
  # reading loop above can widen what this line deletes.
  [[ "$tag" =~ $EDGE_PATTERN ]] || fail "'$tag' is not an edge tag"
  printf '%s\n' "$tag"
  if [[ -n "$DELETE" ]]; then
    gh release delete "$tag" --cleanup-tag --yes \
      || echo "could not delete $tag; a concurrent edge run may already have" >&2
  fi
done <<< "$doomed"
