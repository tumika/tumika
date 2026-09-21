#!/usr/bin/env bash
# Fixture tests for edge-version.sh.
#
# Every case writes its own release.yaml, so what the script reads is decided
# here. The label it prints is held against the daemon's own pattern, copied
# below from releaseLabelPattern in
# source/daemon/internal/platform/release/bom.go: a binary stamped with a label
# the daemon rejects turns every update check it makes into an error, and
# nothing between here and that binary looks at the label again.
#
# Usage: scripts/edge-version_test.sh
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
SCRIPT="$HERE/edge-version.sh"

# releaseLabelPattern, in POSIX ERE. Go's (?:...) is a non-capturing group,
# which bash's =~ has no equivalent of and does not need.
LABEL_PATTERN='^([0-9]{4}\.[0-9]{2}\.[0-9]{2}(-beta\.[0-9]{1,6})?|edge\.[0-9]{1,10})$'
# Semver 2.0.0, which is what the updater compares component versions with.
NUM='(0|[1-9][0-9]*)'
PRE_ID='(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)'
VERSION_PATTERN="^${NUM}\\.${NUM}\\.${NUM}(-${PRE_ID}(\\.${PRE_ID})*)?\$"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

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

RUN_OUT=""
RUN_STATUS=0
run_version() {
  set +e
  RUN_OUT=$("$SCRIPT" "$@" 2>&1)
  RUN_STATUS=$?
  set -e
}

report_failure() {
  echo "FAIL - $1: $2"
  printf '%s\n' "$RUN_OUT" | sed 's/^/       /'
  failures=$((failures + 1))
}

# The case passes, printing exactly the two KEY=VALUE lines, and both values
# hold against the patterns above.
expect_values() {
  local case="$1" want_version="$2" want_label="$3"
  local want="TUMIKA_DAEMON_VERSION=$want_version
TUMIKA_RELEASE=$want_label"
  if [[ "$RUN_STATUS" -ne 0 ]]; then
    report_failure "$case" "expected a pass, got exit $RUN_STATUS"
  elif [[ "$RUN_OUT" != "$want" ]]; then
    report_failure "$case" "expected exactly '$want'"
  elif [[ ! "$want_label" =~ $LABEL_PATTERN ]]; then
    report_failure "$case" "'$want_label' is not a label the daemon accepts"
  elif [[ ! "$want_version" =~ $VERSION_PATTERN ]]; then
    report_failure "$case" "'$want_version' is not valid semver"
  else
    echo "ok   - $case"
  fi
}

expect_fail() {
  local case="$1" want="$2"
  if [[ "$RUN_STATUS" -eq 0 ]]; then
    report_failure "$case" "expected a failure, got exit 0"
  elif [[ "$RUN_OUT" != *"$want"* ]]; then
    report_failure "$case" "expected a message containing '$want'"
  else
    echo "ok   - $case"
  fi
}

# The ordinary run: the calendar file's daemon version, carrying the run number.
write_release "$WORK/plain/release.yaml" 2026.09.01 "daemon 0.0.1"
run_version 42 "$WORK/plain/release.yaml"
expect_values plain 0.0.1-edge.42 edge.42

# A beta component version already carries a prerelease segment, and the edge
# suffix extends it rather than replacing it.
write_release "$WORK/beta/release.yaml" 2026.09.01-beta.1 "daemon 0.0.2-beta.1"
run_version 7 "$WORK/beta/release.yaml"
expect_values beta 0.0.2-beta.1-edge.7 edge.7

# The largest run number the daemon's label pattern accepts, and the first one
# it does not.
write_release "$WORK/wide/release.yaml" 2026.09.01 "daemon 0.0.1"
run_version 1234567890 "$WORK/wide/release.yaml"
expect_values wide-run-number 0.0.1-edge.1234567890 edge.1234567890

run_version 12345678901 "$WORK/wide/release.yaml"
expect_fail too-wide-run-number "is not a release label the daemon accepts"

# A zero-padded run number is not a semver prerelease identifier, so it is
# refused rather than stamped into a version the updater cannot compare.
run_version 007 "$WORK/wide/release.yaml"
expect_fail zero-padded-run-number "is not valid semver"

run_version v3 "$WORK/wide/release.yaml"
expect_fail non-numeric-run-number "is not a run number"

# An empty run number is an argument like any other: reading past it would take
# the release.yaml path for the run number.
run_version "" "$WORK/wide/release.yaml"
expect_fail empty-run-number "'' is not a run number"

run_version
expect_fail no-run-number "usage:"

run_version 3 "$WORK/wide/release.yaml" extra
expect_fail too-many-arguments "usage:"

# Every component carries the suffix, not only the daemon: the BOM generator
# appends it to each component version it resolves, and a file naming one of
# them without it describes assets under a name nothing publishes.
write_release "$WORK/multi/release.yaml" 2026.09.01 "daemon 0.0.1" "desktop 0.0.3"
run_version --write "$WORK/multi/out.yaml" 9 "$WORK/multi/release.yaml"
expect_values multi-component 0.0.1-edge.9 edge.9
if [[ "$(grep -c -- '-edge\.9' "$WORK/multi/out.yaml")" -ne 2 ]]; then
  report_failure multi-component "both component versions should carry the suffix"
else
  echo "ok   - multi-component suffixes every component"
fi

# The written file is what the edge desktop build reads its own component version
# out of, through the same script the calendar release reads it with: that value
# names the app's assets and is stamped into the bundle.
if [[ "$("$HERE/release-component-version.sh" desktop "$WORK/multi/out.yaml")" != "0.0.3-edge.9" ]]; then
  report_failure multi-component "the written file should name the edge desktop version"
else
  echo "ok   - multi-component names the edge desktop version"
fi

# The written file keeps the calendar label. An edge build has no calendar
# release of its own, and the generator reads its edge label from the tag.
if ! grep -qx 'release: 2026.09.01' "$WORK/multi/out.yaml"; then
  report_failure multi-component "the written file should keep the calendar label"
else
  echo "ok   - multi-component keeps the calendar release label"
fi

# --write over the file it read leaves a file the same two scripts still read,
# which is how the edge workflow prepares the asset goreleaser attaches.
cp "$WORK/multi/release.yaml" "$WORK/inplace.yaml"
run_version --write "$WORK/inplace.yaml" 11 "$WORK/inplace.yaml"
expect_values write-in-place 0.0.1-edge.11 edge.11
if [[ "$("$HERE/release-component-version.sh" daemon "$WORK/inplace.yaml")" != "0.0.1-edge.11" ]]; then
  report_failure write-in-place "the rewritten file should name the edge daemon version"
else
  echo "ok   - write-in-place is still a file release-component-version.sh reads"
fi

# Comments and indentation survive, because the file is published as an asset
# and read by people as well as by the generator.
cat > "$WORK/comments.yaml" <<'YAML'
# The single source of the release label.
release: 2026.09.01 # the September hotfix
components:
  daemon: 0.0.1 # the daemon itself
YAML
run_version --write "$WORK/comments-out.yaml" 5 "$WORK/comments.yaml"
expect_values comments 0.0.1-edge.5 edge.5
if ! grep -qx '  daemon: 0.0.1-edge.5 # the daemon itself' "$WORK/comments-out.yaml"; then
  report_failure comments "the component's inline comment should survive"
else
  echo "ok   - comments survive the rewrite"
fi

# Nothing else in this pipeline knows how to build a daemon asset name.
write_release "$WORK/no-daemon/release.yaml" 2026.09.01 "desktop 0.0.3"
run_version 3 "$WORK/no-daemon/release.yaml"
expect_fail no-daemon-component "no component 'daemon'"

write_release "$WORK/bad-label/release.yaml" 2026.9.1 "daemon 0.0.1"
run_version 3 "$WORK/bad-label/release.yaml"
expect_fail bad-input-label "does not name a release label"

run_version 3 "$WORK/missing/release.yaml"
expect_fail missing-file "no $WORK/missing/release.yaml"

if [[ "$failures" -ne 0 ]]; then
  echo "$failures case(s) failed" >&2
  exit 1
fi
echo "all cases passed"
