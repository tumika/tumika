#!/usr/bin/env bash
# Fixture tests for edge-check-artifact.sh.
#
# Each case builds a directory standing in for the artifact the build job
# uploaded, and asserts what the check says about it. The refusals are the point:
# this script is the only thing between a branch's build output and
# `gh release create` running with a write token, so every case here is a file
# somebody could put in the artifact and should not be able to publish.
#
# Usage: scripts/edge-check-artifact_test.sh
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
SCRIPT="$HERE/edge-check-artifact.sh"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

failures=0

CASE_DIR=""
# A fresh directory holding the full, good asset set for edge-<run> at <version>.
good_set() {
  local version="$1"
  CASE_DIR="$(mktemp -d "$WORK/case.XXXXXX")"
  local target os arch
  for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
    os="${target%%/*}"; arch="${target##*/}"
    printf 'binary\n' > "$CASE_DIR/tumika_${version}_${os}_${arch}"
    printf 'archive\n' > "$CASE_DIR/tumika_${version}_${os}_${arch}.tar.gz"
  done
  printf 'deadbeef  tumika\n' > "$CASE_DIR/checksums.txt"
  printf 'release: 2026.09.01\n' > "$CASE_DIR/release.yaml"
}

RUN_OUT=""
RUN_ERR=""
RUN_STATUS=0
run_check() {
  set +e
  RUN_OUT=$("$SCRIPT" "$@" 2>"$WORK/err")
  RUN_STATUS=$?
  set -e
  RUN_ERR=$(cat "$WORK/err")
}

report_failure() {
  echo "FAIL - $1: $2"
  printf '%s\n' "$RUN_OUT" "$RUN_ERR" | sed 's/^/       /'
  failures=$((failures + 1))
}

expect_pass() {
  local case="$1"
  if [[ "$RUN_STATUS" -ne 0 ]]; then
    report_failure "$case" "expected a pass, got exit $RUN_STATUS"
  else
    echo "ok   - $case"
  fi
}

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

good_set 0.0.1-edge.7
run_check "$CASE_DIR" 7
expect_pass the-good-set

# A component version that is itself a prerelease carries both suffixes, and the
# edge one is still the last.
good_set 0.1.0-beta.1-edge.7
run_check "$CASE_DIR" 7
expect_pass a-beta-component-version

# The run number is the caller's, from github.run_number. Assets from another
# run are assets this release must not publish under its own tag.
good_set 0.0.1-edge.6
run_check "$CASE_DIR" 7
expect_fail wrong-run-number "is not an edge-7 release asset"

good_set 0.0.1-edge.7
printf 'x\n' > "$CASE_DIR/install.sh"
run_check "$CASE_DIR" 7
expect_fail stray-file "'install.sh' is not an edge-7 release asset"

# A name that is an asset name with a directory component in it. The allow-list
# is anchored, so it is refused on the name alone.
good_set 0.0.1-edge.7
printf 'x\n' > "$CASE_DIR/..tumika_0.0.1-edge.7_linux_amd64"
run_check "$CASE_DIR" 7
expect_fail dot-dot-name "is not an edge-7 release asset"

good_set 0.0.1-edge.7
mkdir -p "$CASE_DIR/nested"
printf 'x\n' > "$CASE_DIR/nested/tumika_0.0.1-edge.7_linux_amd64"
run_check "$CASE_DIR" 7
expect_fail nested-directory "nested entries"

# `gh release create` follows a symlink and uploads its target, under a name
# this allow-list accepts.
good_set 0.0.1-edge.7
rm "$CASE_DIR/checksums.txt"
ln -s /etc/passwd "$CASE_DIR/checksums.txt"
run_check "$CASE_DIR" 7
expect_fail symlink "is a symlink"

good_set 0.0.1-edge.7
rm "$CASE_DIR/checksums.txt"
run_check "$CASE_DIR" 7
expect_fail missing-checksums "no checksums.txt"

good_set 0.0.1-edge.7
rm "$CASE_DIR/release.yaml"
run_check "$CASE_DIR" 7
expect_fail missing-release-yaml "no release.yaml"

good_set 0.0.1-edge.7
rm "$CASE_DIR/tumika_0.0.1-edge.7_linux_arm64"
run_check "$CASE_DIR" 7
expect_fail missing-a-raw-binary "no 'tumika_0.0.1-edge.7_linux_arm64' in the artifact"

good_set 0.0.1-edge.7
rm "$CASE_DIR/tumika_0.0.1-edge.7_darwin_amd64.tar.gz"
run_check "$CASE_DIR" 7
expect_fail missing-an-archive "no 'tumika_0.0.1-edge.7_darwin_amd64.tar.gz' in the artifact"

# Two versions in one artifact describe two builds, and nothing in the release
# would say which one a given asset came from.
good_set 0.0.1-edge.7
printf 'binary\n' > "$CASE_DIR/tumika_0.0.2-edge.7_linux_amd64"
run_check "$CASE_DIR" 7
expect_fail mixed-versions "more than one component version"

CASE_DIR="$(mktemp -d "$WORK/case.XXXXXX")"
printf 'deadbeef  tumika\n' > "$CASE_DIR/checksums.txt"
printf 'release: 2026.09.01\n' > "$CASE_DIR/release.yaml"
run_check "$CASE_DIR" 7
expect_fail no-assets-at-all "carries no release assets"

run_check "$WORK/does-not-exist" 7
expect_fail missing-directory "no directory"

good_set 0.0.1-edge.7
run_check "$CASE_DIR" seven
expect_fail bad-run-number "is not a run number"

run_check "$CASE_DIR"
expect_fail missing-argument "usage:"

if [[ "$failures" -ne 0 ]]; then
  echo "$failures case(s) failed" >&2
  exit 1
fi
echo "all cases passed"
