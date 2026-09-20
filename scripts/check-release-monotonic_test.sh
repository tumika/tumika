#!/usr/bin/env bash
# Fixture tests for check-release-monotonic.sh, and for the one thing
# verify-release-assets.sh does with the list that script reports.
#
# `gh` is stubbed on PATH: each case names a file of "tag<TAB>isDraft<TAB>
# publishedAt" lines (what `gh release list --jq` emits) and a directory of
# assets keyed by tag, so which release the script picks and what it does with
# the version it finds are both decided here rather than by a network call.
#
# Usage: scripts/check-release-monotonic_test.sh
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
SCRIPT="$HERE/check-release-monotonic.sh"
VERIFY="$HERE/verify-release-assets.sh"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

mkdir -p "$WORK/bin"
cat > "$WORK/bin/gh" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
[[ "${1:-}" == "release" ]] || { echo "stub gh: unexpected command ${1:-}" >&2; exit 1; }
case "${2:-}" in
  list)
    [[ ! -f "$GH_STUB_CASE/fail-list" ]] || { echo "stub gh: list failed" >&2; exit 1; }
    cat "$GH_STUB_LIST"
    ;;
  view)
    [[ ! -f "$GH_STUB_CASE/fail-view" ]] || { echo "stub gh: view failed" >&2; exit 1; }
    [[ "${4:-} ${5:-}" == "--json assets" ]] || { echo "stub gh: unexpected view arguments" >&2; exit 1; }
    echo "checksums.txt"
    if [[ -f "$GH_STUB_ASSETS/$3/release.yaml" ]]; then echo "release.yaml"; fi
    ;;
  download)
    [[ ! -f "$GH_STUB_CASE/fail-download" ]] || { echo "stub gh: download failed" >&2; exit 1; }
    tag="$3"
    dir=""
    shift 3
    while [[ $# -gt 0 ]]; do
      case "$1" in
        --dir) dir="$2"; shift 2 ;;
        *) shift ;;
      esac
    done
    src="$GH_STUB_ASSETS/$tag/release.yaml"
    [[ -f "$src" ]] || { echo "release not found or no asset matched the pattern" >&2; exit 1; }
    cp "$src" "$dir/release.yaml"
    ;;
  *)
    echo "stub gh: unexpected subcommand ${2:-}" >&2
    exit 1
    ;;
esac
STUB
chmod +x "$WORK/bin/gh"
PATH="$WORK/bin:$PATH"
export PATH

failures=0

# Writes a release.yaml naming <label> and one "<name> <version>" pair per
# remaining argument.
write_release() {
  local path="$1" label="$2"
  shift 2
  mkdir -p "$(dirname "$path")"
  {
    echo "release: $label"
    echo "components:"
    local entry
    for entry in "$@"; do
      echo "  ${entry% *}: ${entry#* }"
    done
  } > "$path"
}

# Registers the release.yaml a published release carries.
write_asset() {
  local case="$1" tag="$2"
  shift 2
  write_release "$WORK/$case/assets/$tag/release.yaml" "${tag#v}" "$@"
}

# Registers the release list the stub returns, one "tag isDraft publishedAt"
# triple per argument.
write_list() {
  local case="$1"
  shift
  mkdir -p "$WORK/$case"
  : > "$WORK/$case/list.tsv"
  local entry
  for entry in "$@"; do
    # shellcheck disable=SC2086 # the triple is deliberately split on spaces
    printf '%s\t%s\t%s\n' $entry >> "$WORK/$case/list.tsv"
  done
}

RUN_OUT=""
RUN_STATUS=0
run_case() {
  local case="$1"
  set +e
  RUN_OUT=$(GH_STUB_CASE="$WORK/$case" GH_STUB_LIST="$WORK/$case/list.tsv" GH_STUB_ASSETS="$WORK/$case/assets" \
    GITHUB_OUTPUT="$WORK/$case/github-output" \
    "$SCRIPT" "$WORK/$case/release.yaml" 2>&1)
  RUN_STATUS=$?
  set -e
}

report_failure() {
  echo "FAIL - $1: $2"
  printf '%s\n' "$RUN_OUT" | sed 's/^/       /'
  failures=$((failures + 1))
}

# The case passes, and says <want>.
expect_pass() {
  local case="$1" want="$2"
  run_case "$case"
  if [[ "$RUN_STATUS" -ne 0 ]]; then
    report_failure "$case" "expected a pass, got exit $RUN_STATUS"
  elif [[ "$RUN_OUT" != *"$want"* ]]; then
    report_failure "$case" "expected output containing '$want'"
  else
    echo "ok   - $case"
  fi
}

# The case passes, says <want>, and reports exactly <changed> as the components
# the release rebuilds — both on stdout and as the step output a workflow reads.
expect_changed() {
  local case="$1" want="$2" changed="$3"
  run_case "$case"
  local printed output
  printed=$(grep '^changed=' <<< "$RUN_OUT" || true)
  output=$(grep '^changed=' "$WORK/$case/github-output" 2>/dev/null || true)
  if [[ "$RUN_STATUS" -ne 0 ]]; then
    report_failure "$case" "expected a pass, got exit $RUN_STATUS"
  elif [[ "$RUN_OUT" != *"$want"* ]]; then
    report_failure "$case" "expected output containing '$want'"
  elif [[ "$printed" != "changed=$changed" ]]; then
    report_failure "$case" "expected the line 'changed=$changed', got '$printed'"
  elif [[ "$output" != "changed=$changed" ]]; then
    report_failure "$case" "expected 'changed=$changed' in \$GITHUB_OUTPUT, got '$output'"
  else
    echo "ok   - $case"
  fi
}

# The case fails, and says <want>.
expect_fail() {
  local case="$1" want="$2"
  run_case "$case"
  if [[ "$RUN_STATUS" -eq 0 ]]; then
    report_failure "$case" "expected a failure, got exit 0"
  elif [[ "$RUN_OUT" != *"$want"* ]]; then
    report_failure "$case" "expected output containing '$want'"
  else
    echo "ok   - $case"
  fi
}

# No published release at all: the first release of every component, and every
# one of them is built.
write_list no-previous
write_release "$WORK/no-previous/release.yaml" 2026.09.01 "daemon 0.0.1"
expect_changed no-previous "no published release to compare against" "daemon"

# The ordinary pass: the version advances, so the component is rebuilt.
write_list greater "v2026.08.00 false 2026-08-01T10:00:00Z"
write_asset greater v2026.08.00 "daemon 0.0.1"
write_release "$WORK/greater/release.yaml" 2026.09.01 "daemon 0.0.2"
expect_changed greater "daemon 0.0.1 -> 0.0.2" "daemon"

# An equal version is what carrying a component over unchanged looks like: it
# passes, and the component is not among those the release rebuilds.
write_list equal "v2026.08.00 false 2026-08-01T10:00:00Z"
write_asset equal v2026.08.00 "daemon 0.0.2"
write_release "$WORK/equal/release.yaml" 2026.09.01 "daemon 0.0.2"
expect_changed equal "daemon 0.0.2 unchanged since v2026.08.00" ""

write_list lower "v2026.08.00 false 2026-08-01T10:00:00Z"
write_asset lower v2026.08.00 "daemon 0.0.3"
write_release "$WORK/lower/release.yaml" 2026.09.01 "daemon 0.0.2"
expect_fail lower "is 0.0.2 in"

# A hotfix: one component advances, the other is carried over, and only the
# first is reported.
write_list one-unchanged "v2026.08.00 false 2026-08-01T10:00:00Z"
write_asset one-unchanged v2026.08.00 "daemon 0.0.2" "desktop 0.0.1"
write_release "$WORK/one-unchanged/release.yaml" 2026.09.01 "daemon 0.0.3" "desktop 0.0.1"
expect_changed one-unchanged "desktop 0.0.1 unchanged since v2026.08.00" "daemon"

# A beta's suffix orders it below the stable version of the same core, in both
# directions.
write_list beta-to-stable "v2026.09.00-beta.1 false 2026-09-01T10:00:00Z"
write_asset beta-to-stable v2026.09.00-beta.1 "daemon 0.0.2-beta.1"
write_release "$WORK/beta-to-stable/release.yaml" 2026.09.01 "daemon 0.0.2"
expect_pass beta-to-stable "daemon 0.0.2-beta.1 -> 0.0.2"

write_list beta-to-beta "v2026.09.00-beta.1 false 2026-09-01T10:00:00Z"
write_asset beta-to-beta v2026.09.00-beta.1 "daemon 0.0.2-beta.1"
write_release "$WORK/beta-to-beta/release.yaml" 2026.09.01-beta.2 "daemon 0.0.2-beta.2"
expect_pass beta-to-beta "daemon 0.0.2-beta.1 -> 0.0.2-beta.2"

write_list stable-to-beta "v2026.09.00 false 2026-09-01T10:00:00Z"
write_asset stable-to-beta v2026.09.00 "daemon 0.0.2"
write_release "$WORK/stable-to-beta/release.yaml" 2026.09.01-beta.1 "daemon 0.0.2-beta.1"
expect_fail stable-to-beta "is 0.0.2-beta.1 in"

# The in-flight release is a draft, and a draft is not a predecessor.
write_list draft-ignored \
  "v2026.09.02 true 2026-09-20T10:00:00Z" \
  "v2026.08.00 false 2026-08-01T10:00:00Z"
write_asset draft-ignored v2026.09.02 "daemon 9.9.9"
write_asset draft-ignored v2026.08.00 "daemon 0.0.1"
write_release "$WORK/draft-ignored/release.yaml" 2026.09.01 "daemon 0.0.2"
expect_pass draft-ignored "past v2026.08.00"

# Edge builds tag themselves outside the calendar shape and move their component
# versions backwards by design.
write_list edge-ignored \
  "edge-42 false 2026-09-19T10:00:00Z" \
  "v2026.08.00 false 2026-08-01T10:00:00Z"
write_asset edge-ignored edge-42 "daemon 9.9.9"
write_asset edge-ignored v2026.08.00 "daemon 0.0.1"
write_release "$WORK/edge-ignored/release.yaml" 2026.09.01 "daemon 0.0.2"
expect_pass edge-ignored "past v2026.08.00"

# Re-running a release whose draft was already published must not compare it
# against itself.
write_list own-tag-ignored \
  "v2026.09.01 false 2026-09-20T10:00:00Z" \
  "v2026.08.00 false 2026-08-01T10:00:00Z"
write_asset own-tag-ignored v2026.09.01 "daemon 0.0.2"
write_asset own-tag-ignored v2026.08.00 "daemon 0.0.1"
write_release "$WORK/own-tag-ignored/release.yaml" 2026.09.01 "daemon 0.0.2"
expect_pass own-tag-ignored "past v2026.08.00"

# A release cut before release.yaml became an asset has no version to compare.
write_list missing-asset "v2026.08.00 false 2026-08-01T10:00:00Z"
mkdir -p "$WORK/missing-asset/assets"
write_release "$WORK/missing-asset/release.yaml" 2026.09.01 "daemon 0.0.2" "desktop 0.0.1"
expect_changed missing-asset "carries no release.yaml asset" "daemon,desktop"

# The predecessor is the most recently published one, whatever order the list
# arrives in.
write_list latest-published \
  "v2026.08.00 false 2026-08-01T10:00:00Z" \
  "v2026.08.02 false 2026-08-20T10:00:00Z" \
  "v2026.08.01 false 2026-08-10T10:00:00Z"
write_asset latest-published v2026.08.00 "daemon 9.9.9"
write_asset latest-published v2026.08.01 "daemon 9.9.9"
write_asset latest-published v2026.08.02 "daemon 0.0.5"
write_release "$WORK/latest-published/release.yaml" 2026.09.01 "daemon 0.0.6"
expect_pass latest-published "past v2026.08.02"

# A component the previous release did not name has nothing to advance past, and
# has to be built.
write_list new-component "v2026.08.00 false 2026-08-01T10:00:00Z"
write_asset new-component v2026.08.00 "daemon 0.0.1"
write_release "$WORK/new-component/release.yaml" 2026.09.01 "daemon 0.0.2" "desktop 0.0.1"
expect_changed new-component "desktop 0.0.1 is new since v2026.08.00" "daemon,desktop"

# An API or network failure is not evidence that the previous release lacks the
# asset; each failing call fails the gate.
write_list list-fails "v2026.08.00 false 2026-08-01T10:00:00Z"
write_asset list-fails v2026.08.00 "daemon 0.0.1"
write_release "$WORK/list-fails/release.yaml" 2026.09.01 "daemon 0.0.2"
touch "$WORK/list-fails/fail-list"
expect_fail list-fails "could not list releases"

write_list view-fails "v2026.08.00 false 2026-08-01T10:00:00Z"
write_asset view-fails v2026.08.00 "daemon 0.0.1"
write_release "$WORK/view-fails/release.yaml" 2026.09.01 "daemon 0.0.2"
touch "$WORK/view-fails/fail-view"
expect_fail view-fails "could not read the assets of v2026.08.00"

write_list download-fails "v2026.08.00 false 2026-08-01T10:00:00Z"
write_asset download-fails v2026.08.00 "daemon 0.0.1"
write_release "$WORK/download-fails/release.yaml" 2026.09.01 "daemon 0.0.2"
touch "$WORK/download-fails/fail-download"
expect_fail download-fails "could not download release.yaml from v2026.08.00"

# The other half of the contract: verify-release-assets.sh takes the list this
# script reports through TUMIKA_CHANGED_COMPONENTS, and asks for daemon assets
# only when the daemon is in it. The dist directory does not exist in either
# case, which is what makes the distinction visible: the skipping run never looks
# for one, and the run that does is stopped by its absence.
run_verify() {
  local case="$1"
  shift
  set +e
  RUN_OUT=$(env "$@" "$VERIFY" "$WORK/$case/dist" 2>&1)
  RUN_STATUS=$?
  set -e
}

run_verify verify-daemon-unchanged TUMIKA_CHANGED_COMPONENTS=desktop
if [[ "$RUN_STATUS" -ne 0 ]]; then
  report_failure verify-daemon-unchanged "expected a pass, got exit $RUN_STATUS"
elif [[ "$RUN_OUT" != *"does not name the daemon"* || "$RUN_OUT" != *PASS* ]]; then
  report_failure verify-daemon-unchanged "expected the daemon to be reported as carried over"
else
  echo "ok   - verify-daemon-unchanged"
fi

run_verify verify-daemon-changed TUMIKA_CHANGED_COMPONENTS=daemon,desktop
if [[ "$RUN_STATUS" -eq 0 ]]; then
  report_failure verify-daemon-changed "expected a failure, got exit 0"
elif [[ "$RUN_OUT" != *"artifacts.json"* ]]; then
  report_failure verify-daemon-changed "expected the daemon's assets to be demanded"
else
  echo "ok   - verify-daemon-changed"
fi

# An absent list is not an empty one: nothing to skip on, so the daemon's assets
# are demanded. An empty list is refused rather than treated as "skip
# everything", which would print PASS without asserting anything.
run_verify verify-no-list
if [[ "$RUN_STATUS" -eq 0 ]]; then
  report_failure verify-no-list "expected a failure, got exit 0"
elif [[ "$RUN_OUT" != *"artifacts.json"* ]]; then
  report_failure verify-no-list "expected the daemon's assets to be demanded"
else
  echo "ok   - verify-no-list"
fi

run_verify verify-empty-list TUMIKA_CHANGED_COMPONENTS=
if [[ "$RUN_STATUS" -eq 0 ]]; then
  report_failure verify-empty-list "expected a failure, got exit 0"
elif [[ "$RUN_OUT" != *"names no component"* ]]; then
  report_failure verify-empty-list "expected the empty list to be refused"
else
  echo "ok   - verify-empty-list"
fi

if [[ "$failures" -ne 0 ]]; then
  echo "$failures case(s) failed" >&2
  exit 1
fi
echo "all cases passed"
