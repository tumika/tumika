#!/usr/bin/env bash
# Fixture tests for assemble-site.sh.
#
# Each case builds a BOM tree, an installer stand-in and a static directory from
# scratch, so what the script accepts and what it refuses is decided here rather
# than by whatever the last real publish run happened to produce.
#
# Usage: scripts/assemble-site_test.sh
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
SCRIPT="$HERE/assemble-site.sh"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

failures=0

# Builds a complete, valid case under $WORK/<case>: a BOM tree with one channel
# head and one release, an installer, and the repo's own static files. Each case
# then removes or corrupts the one thing it is about.
new_case() {
  local case="$1"
  local dir="$WORK/$case"
  mkdir -p "$dir/bom/channels" "$dir/bom/releases" "$dir/static"

  echo '{"channel":"stable"}' > "$dir/bom/channels/stable.json"
  echo 'signature' > "$dir/bom/channels/stable.json.sig"
  echo '{"release":"2026.09.00"}' > "$dir/bom/releases/2026.09.00.json"
  echo 'signature' > "$dir/bom/releases/2026.09.00.json.sig"

  printf '#!/bin/sh\necho install\n' > "$dir/install-daemon.sh"
  chmod +x "$dir/install-daemon.sh"

  cp "$HERE/site/CNAME" "$HERE/site/index.html" "$dir/static/"
}

RUN_OUT=""
RUN_STATUS=0
run_case() {
  local case="$1"
  local dir="$WORK/$case"
  set +e
  RUN_OUT=$("$SCRIPT" "$dir/bom" "$dir/install-daemon.sh" "$dir/static" "$dir/site" 2>&1)
  RUN_STATUS=$?
  set -e
}

report_failure() {
  echo "FAIL - $1: $2"
  printf '%s\n' "$RUN_OUT" | sed 's/^/       /'
  failures=$((failures + 1))
}

# The case assembles, and says <want>.
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

# The case is refused, and says <want>.
expect_fail() {
  local case="$1" want="$2"
  run_case "$case"
  if [[ "$RUN_STATUS" -eq 0 ]]; then
    report_failure "$case" "expected a refusal, got exit 0"
  elif [[ "$RUN_OUT" != *"$want"* ]]; then
    report_failure "$case" "expected output containing '$want'"
  else
    echo "ok   - $case"
  fi
}

# A complete set of inputs produces the tree the host serves.
new_case complete
expect_pass complete "1 channel head(s)"
site="$WORK/complete/site"
for want in CNAME index.html install-daemon.sh \
  channels/stable.json channels/stable.json.sig \
  releases/2026.09.00.json releases/2026.09.00.json.sig; do
  if [[ ! -f "$site/$want" ]]; then
    echo "FAIL - complete: the assembled site has no $want"
    failures=$((failures + 1))
  fi
done
if [[ ! -x "$site/install-daemon.sh" ]]; then
  echo "FAIL - complete: install-daemon.sh is not executable"
  failures=$((failures + 1))
fi
if [[ "$(cat "$site/CNAME")" != "get.tumika.org" ]]; then
  echo "FAIL - complete: CNAME does not name get.tumika.org"
  failures=$((failures + 1))
fi

# The installer is named by path because it is produced elsewhere; an absent one
# is the case worth saying plainly.
new_case no-installer
rm "$WORK/no-installer/install-daemon.sh"
expect_fail no-installer "is not a file"

new_case no-cname
rm "$WORK/no-cname/static/CNAME"
expect_fail no-cname "carries no CNAME"

# Pages reads an empty CNAME as "no custom domain" rather than as an error.
new_case empty-cname
: > "$WORK/empty-cname/static/CNAME"
expect_fail empty-cname "names 0 hosts"

new_case two-hosts
printf 'get.tumika.org\nwww.tumika.org\n' > "$WORK/two-hosts/static/CNAME"
expect_fail two-hosts "names 2 hosts"

new_case no-index
rm "$WORK/no-index/static/index.html"
expect_fail no-index "carries no index.html"

new_case unsigned-channel
rm "$WORK/unsigned-channel/bom/channels/stable.json.sig"
expect_fail unsigned-channel "channels/stable.json has no matching .sig"

new_case unsigned-release
rm "$WORK/unsigned-release/bom/releases/2026.09.00.json.sig"
expect_fail unsigned-release "releases/2026.09.00.json has no matching .sig"

new_case no-channels
rm -r "$WORK/no-channels/bom/channels"
expect_fail no-channels "there is nothing to serve"

new_case no-bom
rm -r "$WORK/no-bom/bom"
expect_fail no-bom "is not a directory"

new_case no-static
rm -r "$WORK/no-static/static"
expect_fail no-static "is not a directory"

# A second run into the same directory would carry an earlier run's leftovers
# past every check above.
new_case out-exists
mkdir -p "$WORK/out-exists/site"
expect_fail out-exists "already exists"

set +e
usage=$("$SCRIPT" one two three 2>&1)
usage_status=$?
set -e
if [[ "$usage_status" -eq 0 || "$usage" != *"usage:"* ]]; then
  echo "FAIL - arity: three arguments should be refused with a usage line"
  failures=$((failures + 1))
else
  echo "ok   - arity"
fi

if [[ "$failures" -ne 0 ]]; then
  echo "$failures case(s) failed" >&2
  exit 1
fi
echo "all cases passed"
