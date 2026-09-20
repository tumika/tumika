#!/usr/bin/env bash
# Fixture tests for edge-prune.sh.
#
# Each case is a release list as `gh release list --jq` emits it, and what the
# script says should go. `gh` is stubbed on PATH and records every call, so the
# --delete cases assert what would have been deleted rather than deleting
# anything: the one thing this script must never get wrong is which tag it
# hands to `gh release delete`.
#
# Usage: scripts/edge-prune_test.sh
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
SCRIPT="$HERE/edge-prune.sh"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

mkdir -p "$WORK/bin"
cat > "$WORK/bin/gh" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$GH_STUB_LOG"
[[ "${1:-} ${2:-}" == "release delete" ]] || { echo "stub gh: unexpected command $*" >&2; exit 1; }
[[ "${4:-} ${5:-}" == "--cleanup-tag --yes" ]] || { echo "stub gh: unexpected flags $*" >&2; exit 1; }
[[ ! -f "$GH_STUB_FAIL" ]] || { echo "stub gh: delete failed" >&2; exit 1; }
STUB
chmod +x "$WORK/bin/gh"
PATH="$WORK/bin:$PATH"
export PATH
export GH_STUB_LOG="$WORK/gh.log"
export GH_STUB_FAIL="$WORK/gh-fail"

failures=0

# One "tag isDraft createdAt" triple per argument, as tab-separated lines.
tsv() {
  local entry
  for entry in "$@"; do
    # shellcheck disable=SC2086 # the triple is deliberately split on spaces
    printf '%s\t%s\t%s\n' $entry
  done
}

RUN_OUT=""
RUN_ERR=""
RUN_STATUS=0
# Runs the script over <input> with the remaining arguments as its flags.
run_prune() {
  local input="$1"
  shift
  : > "$GH_STUB_LOG"
  set +e
  RUN_OUT=$(printf '%s' "$input" | "$SCRIPT" "$@" 2>"$WORK/err")
  RUN_STATUS=$?
  set -e
  RUN_ERR=$(cat "$WORK/err")
}

report_failure() {
  echo "FAIL - $1: $2"
  printf '%s\n' "$RUN_OUT" "$RUN_ERR" | sed 's/^/       /'
  failures=$((failures + 1))
}

# The case passes, printing exactly <want> (a newline-separated list of tags).
expect_tags() {
  local case="$1" want="$2"
  if [[ "$RUN_STATUS" -ne 0 ]]; then
    report_failure "$case" "expected a pass, got exit $RUN_STATUS"
  elif [[ "$RUN_OUT" != "$want" ]]; then
    report_failure "$case" "expected exactly '$want'"
  else
    echo "ok   - $case"
  fi
}

# The case fails, saying <want>.
expect_fail() {
  local case="$1" want="$2"
  if [[ "$RUN_STATUS" -eq 0 ]]; then
    report_failure "$case" "expected a failure, got exit 0"
  elif [[ "$RUN_ERR" != *"$want"* ]]; then
    report_failure "$case" "expected a message containing '$want'"
  else
    echo "ok   - $case"
  fi
}

# The stub was called exactly for <want> (a newline-separated list of tags).
expect_deleted() {
  local case="$1" want="$2"
  local got
  got=$(sed 's/^release delete \([^ ]*\) .*$/\1/' "$GH_STUB_LOG")
  if [[ "$got" != "$want" ]]; then
    report_failure "$case" "expected exactly '$want' deleted, got '$got'"
  else
    echo "ok   - $case deleted the right tags"
  fi
}

published() { printf 'edge-%s false 2026-09-0%sT10:00:00Z' "$1" "$1"; }

# Nine edge builds, five kept: the four oldest go, newest first.
run_prune "$(tsv "$(published 1)" "$(published 2)" "$(published 3)" "$(published 4)" \
  "$(published 5)" "$(published 6)" "$(published 7)" "$(published 8)" "$(published 9)")"
expect_tags keeps-newest-five "edge-4
edge-3
edge-2
edge-1"

# Fewer than the number kept: nothing to prune, and the exit status still says
# so.
run_prune "$(tsv "$(published 1)" "$(published 2)" "$(published 3)")"
expect_tags fewer-than-kept ""

run_prune ""
expect_tags empty-list ""

# The run number orders the builds, numerically. edge-10 is the newest of these
# six, so the one dropped is edge-2 — a byte comparison would drop edge-10.
run_prune "$(tsv "edge-2 false t" "edge-3 false t" "edge-4 false t" \
  "edge-9 false t" "edge-10 false t" "edge-11 false t")"
expect_tags numeric-order "edge-2"

# A draft is another run's edge build, still being assembled. It is neither
# pruned nor counted, so the five published builds are all kept.
run_prune "$(tsv "edge-7 true t" "$(published 1)" "$(published 2)" "$(published 3)" \
  "$(published 4)" "$(published 5)")"
expect_tags drafts-ignored ""

# Six published builds and a draft: the draft does not displace the oldest.
run_prune "$(tsv "edge-9 true t" "$(published 1)" "$(published 2)" "$(published 3)" \
  "$(published 4)" "$(published 5)" "$(published 6)")"
expect_tags drafts-do-not-displace "edge-1"

# Anything that is not exactly edge-<digits> is invisible to this script, in any
# quantity and at any age.
run_prune "$(tsv "v2026.09.01 false t" "v2026.09.00-beta.1 false t" "edge-abc false t" \
  "edge- false t" "edge-1-beta false t" "EDGE-1 false t" "edge false t" \
  "release-edge-1 false t" "$(published 1)")"
expect_tags non-edge-tags-ignored ""

# The same list with enough edge builds to force a prune: only edge tags are
# named, and only edge tags reach `gh release delete`.
run_prune "$(tsv "v2026.09.01 false t" "v2026.08.00 false t" "edge-abc false t" \
  "$(published 1)" "$(published 2)" "$(published 3)" "$(published 4)" \
  "$(published 5)" "$(published 6)" "$(published 7)")" --delete
expect_tags never-deletes-a-calendar-tag "edge-2
edge-1"
expect_deleted never-deletes-a-calendar-tag "edge-2
edge-1"

# --keep is honoured, and the deletion is the one the printed list names.
run_prune "$(tsv "$(published 1)" "$(published 2)" "$(published 3)")" --keep 2 --delete
expect_tags keep-two "edge-1"
expect_deleted keep-two "edge-1"

# A failed delete is another run having got there first. The prune reports it
# and carries on: failing here would turn every overlapping pair of edge runs
# red, and the next run prunes whatever is still there.
touch "$GH_STUB_FAIL"
run_prune "$(tsv "$(published 1)" "$(published 2)" "$(published 3)")" --keep 2 --delete
expect_tags delete-failure-is-not-fatal "edge-1"
if [[ "$RUN_ERR" != *"could not delete edge-1"* ]]; then
  report_failure delete-failure-is-not-fatal "expected the failure to be reported"
else
  echo "ok   - delete-failure-is-not-fatal reported the failure"
fi
rm -f "$GH_STUB_FAIL"

# A list without its isDraft column would make every draft look published.
run_prune "$(printf 'edge-1\nedge-2\n')"
expect_fail missing-is-draft-column "has no isDraft column"

# The last line of an input that does not end in a newline is a release like any
# other.
run_prune "$(printf 'edge-1\tfalse\tt\nedge-2\tfalse\tt')" --keep 1
expect_tags no-trailing-newline "edge-1"

run_prune "$(tsv "$(published 1)")" --keep x
expect_fail bad-keep "is not a number of releases to keep"

run_prune "$(tsv "$(published 1)")" --wat
expect_fail unknown-argument "unknown argument"

if [[ "$failures" -ne 0 ]]; then
  echo "$failures case(s) failed" >&2
  exit 1
fi
echo "all cases passed"
