#!/usr/bin/env bash
# Stamps the desktop component version into the four files that each carry a
# copy of it, or checks that they all agree.
#
# Tauri names the version its bundle and updater report in tauri.conf.json,
# cargo names the crate's in Cargo.toml and Cargo.lock, and pnpm names the
# package's in package.json. release.yaml is the one source; a copy that
# disagrees ships an app that reports a component version the release never
# named. Cargo.lock is included because `cargo build --locked` fails when the
# crate's entry disagrees with Cargo.toml.
#
# Each edit rewrites one line and leaves every other byte of the file alone
# (bash 3.2 and awk; no jq, no TOML tool). The lines are found by shape:
#   tauri.conf.json, package.json: the first top-level (two-space indent)
#                                  "version" member
#   Cargo.toml:                    the first `version` key under [package]
#   Cargo.lock:                    the `version` line of the [[package]] entry
#                                  whose name is Cargo.toml's [package] name
#
# Usage: scripts/desktop-version.sh [--check] [component-version]
#   component-version  overrides release.yaml, for a suffixed build such as
#                      0.1.0-edge.7; it must be valid semver
#   --check            writes nothing; fails naming each file whose version
#                      differs from the expected one
#
# Environment: TUMIKA_ROOT is the repository root holding release.yaml and
# source/desktop (default: the parent of this script's directory).
set -euo pipefail

fail() { echo "FAIL: $*" >&2; exit 1; }

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="${TUMIKA_ROOT:-$HERE/..}"

CHECK=0
VERSION=""
for arg in "$@"; do
  case "$arg" in
    --check) CHECK=1 ;;
    -*) fail "unknown option '$arg'; usage: $0 [--check] [component-version]" ;;
    *) [[ -z "$VERSION" ]] || fail "more than one component version given"
       VERSION="$arg" ;;
  esac
done

if [[ -z "$VERSION" ]]; then
  VERSION="$("$HERE/release-component-version.sh" desktop "$ROOT/release.yaml")"
else
  NUM='(0|[1-9][0-9]*)'
  PRE_ID='(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)'
  BUILD_ID='[0-9A-Za-z-]+'
  PATTERN="^${NUM}\\.${NUM}\\.${NUM}(-${PRE_ID}(\\.${PRE_ID})*)?(\\+${BUILD_ID}(\\.${BUILD_ID})*)?\$"
  [[ "$VERSION" =~ $PATTERN ]] || fail "'$VERSION' is not valid semver (X.Y.Z[-prerelease])"
fi

DESKTOP="$ROOT/source/desktop"
CONF="$DESKTOP/src-tauri/tauri.conf.json"
CARGO="$DESKTOP/src-tauri/Cargo.toml"
LOCK="$DESKTOP/src-tauri/Cargo.lock"
PKG="$DESKTOP/package.json"

for f in "$CONF" "$CARGO" "$LOCK" "$PKG"; do
  [[ -f "$f" ]] || fail "no $f"
done

# awk program shared by reading and rewriting. kind is json, toml or lock; with
# set non-empty the matched line is rewritten and the file printed, otherwise
# the matched line's current version is printed. Exactly one line is matched.
# shellcheck disable=SC2016 # awk program; the $ are awk's
EDIT='
  function ver(line,   v) {
    v = line
    sub(/^[^=:]*[=:][[:space:]]*"/, "", v)
    sub(/".*$/, "", v)
    return v
  }
  function rewrite(line,   head) {
    head = line
    sub(/"[^"]*"[^"]*$/, "", head)
    return head "\"" set "\"" (line ~ /,[[:space:]]*$/ ? "," : "")
  }
  {
    hit = 0
    if (!done) {
      if (kind == "json" && $0 ~ /^  "version"[[:space:]]*:/) hit = 1
      else if (kind == "toml") {
        if ($0 ~ /^\[/) pkg = ($0 == "[package]")
        else if (pkg && $0 ~ /^version[[:space:]]*=/) hit = 1
      } else if (kind == "lock") {
        if ($0 ~ /^\[\[package\]\]/) want = 0
        else if ($0 == "name = \"" crate "\"") want = 1
        else if (want && $0 ~ /^version[[:space:]]*=/) hit = 1
      }
    }
    if (hit) {
      done = 1
      if (set == "") { print ver($0); next }
      print rewrite($0)
    } else if (set != "") print
  }
  END { if (!done) exit 3 }
'

# Prints the current version in <file>, or nothing when no line matches.
current() { awk -v kind="$1" -v crate="${CRATE:-}" -v set="" "$EDIT" "$2" || true; }

# Rewrites <file> in place, keeping its mode.
stamp() {
  local tmp
  tmp="$(mktemp)"
  awk -v kind="$1" -v crate="${CRATE:-}" -v set="$VERSION" "$EDIT" "$2" >"$tmp" \
    || { rm -f "$tmp"; fail "no version line found in $2"; }
  cat "$tmp" >"$2"
  rm -f "$tmp"
}

CRATE="$(awk '/^\[/ { p = ($0 == "[package]") } p && /^name[[:space:]]*=/ { v = $0; sub(/^[^"]*"/, "", v); sub(/".*$/, "", v); print v; exit }' "$CARGO")"
[[ -n "$CRATE" ]] || fail "no [package] name in $CARGO"

FILES=("json:$CONF" "toml:$CARGO" "lock:$LOCK" "json:$PKG")

if [[ "$CHECK" -eq 1 ]]; then
  bad=0
  for entry in "${FILES[@]}"; do
    kind="${entry%%:*}"; file="${entry#*:}"
    got="$(current "$kind" "$file")"
    if [[ "$got" != "$VERSION" ]]; then
      echo "FAIL: $file has '${got:-<none>}', expected '$VERSION'" >&2
      bad=1
    fi
  done
  [[ "$bad" -eq 0 ]] || exit 1
  echo "desktop component version $VERSION is consistent"
  exit 0
fi

for entry in "${FILES[@]}"; do
  stamp "${entry%%:*}" "${entry#*:}"
done
echo "stamped desktop component version $VERSION"
