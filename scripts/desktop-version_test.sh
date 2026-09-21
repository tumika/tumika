#!/usr/bin/env bash
# Fixture tests for desktop-version.sh.
#
# Each case builds a small repository root (release.yaml plus the four files
# the script stamps) and points TUMIKA_ROOT at it. The fixtures carry a nested
# "version" member and a second Cargo.lock crate, so a rewrite that hits the
# wrong line is caught by the byte comparison against the expected file.
#
# Usage: scripts/desktop-version_test.sh
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
SCRIPT="$HERE/desktop-version.sh"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

failures=0
ok() { echo "ok   $1"; }
bad() { echo "FAIL $1" >&2; failures=$((failures + 1)); }

# Writes a fixture root at $1 with every copy at version $2.
make_root() {
  local r="$1" v="$2" t="$1/source/desktop/src-tauri"
  mkdir -p "$t"
  printf 'release: 2026.09.01\ncomponents:\n  daemon: 0.0.1\n  desktop: 0.1.0\n' >"$r/release.yaml"
  printf '{\n  "productName": "Tumika",\n  "version": "%s",\n  "bundle": {\n    "version": "9.9.9"\n  }\n}\n' "$v" >"$t/tauri.conf.json"
  printf '{\n  "name": "desktop",\n  "version": "%s",\n  "dependencies": {\n    "react": "^19.0.0"\n  }\n}\n' "$v" >"$r/source/desktop/package.json"
  printf '[package]\nname = "tumika-desktop"\nversion = "%s"\nedition = "2021"\n\n[dependencies]\nserde = { version = "1.0", features = ["derive"] }\n' "$v" >"$t/Cargo.toml"
  printf '[[package]]\nname = "serde"\nversion = "1.0.1"\n\n[[package]]\nname = "tumika-desktop"\nversion = "%s"\ndependencies = [\n "serde",\n]\n\n[[package]]\nname = "zed"\nversion = "2.0.0"\n' "$v" >"$t/Cargo.lock"
}

FILES="source/desktop/src-tauri/tauri.conf.json source/desktop/src-tauri/Cargo.toml source/desktop/src-tauri/Cargo.lock source/desktop/package.json"

# Case: write stamps all four files and changes nothing else.
r="$WORK/write"; make_root "$r" 0.0.5; make_root "$WORK/want" 0.1.0
TUMIKA_ROOT="$r" "$SCRIPT" >/dev/null
same=1
for f in $FILES; do cmp -s "$r/$f" "$WORK/want/$f" || { same=0; bad "write: $f differs from the expected bytes"; }; done
[[ "$same" -eq 1 ]] && ok "write stamps the release.yaml version and touches only the version lines"

# Case: check passes on a consistent tree and writes nothing.
r="$WORK/pass"; make_root "$r" 0.1.0
if TUMIKA_ROOT="$r" "$SCRIPT" --check >/dev/null 2>&1; then ok "check passes when all four agree"; else bad "check failed on a consistent tree"; fi

# Case: check fails naming each file that disagrees, and writes nothing.
for f in $FILES; do
  r="$WORK/drift"; rm -rf "$r"; make_root "$r" 0.1.0
  case "$f" in
    *tauri.conf.json) sed '3s/0.1.0/0.2.0/' "$r/$f" >"$r/x" ;;
    *package.json) sed '3s/0.1.0/0.2.0/' "$r/$f" >"$r/x" ;;
    *Cargo.toml) sed '3s/0.1.0/0.2.0/' "$r/$f" >"$r/x" ;;
    *Cargo.lock) sed '7s/0.1.0/0.2.0/' "$r/$f" >"$r/x" ;;
  esac
  mv "$r/x" "$r/$f"
  cp "$r/$f" "$r/before"
  if out="$(TUMIKA_ROOT="$r" "$SCRIPT" --check 2>&1)"; then
    bad "check passed with $f drifted"
  elif [[ "$out" == *"$f"* && "$(printf '%s\n' "$out" | grep -c FAIL)" -eq 1 ]] && cmp -s "$r/$f" "$r/before"; then
    ok "check names $f alone and writes nothing"
  else
    bad "check output for $f: $out"
  fi
done

# Case: an -edge.N suffix is accepted and lands in all four files.
r="$WORK/edge"; make_root "$r" 0.1.0
TUMIKA_ROOT="$r" "$SCRIPT" 0.1.0-edge.7 >/dev/null
if TUMIKA_ROOT="$r" "$SCRIPT" --check 0.1.0-edge.7 >/dev/null 2>&1 \
   && grep -q '"version": "0.1.0-edge.7"' "$r/source/desktop/package.json" \
   && grep -q '^version = "0.1.0-edge.7"' "$r/source/desktop/src-tauri/Cargo.toml"; then
  ok "an -edge.N suffix is valid in every manifest"
else bad "edge suffix not stamped"; fi

# Case: a non-semver argument is refused.
r="$WORK/junk"; make_root "$r" 0.1.0
if TUMIKA_ROOT="$r" "$SCRIPT" 1.0 >/dev/null 2>&1; then bad "accepted 1.0"; else ok "a non-semver argument is refused"; fi

[[ "$failures" -eq 0 ]] || { echo "$failures failure(s)" >&2; exit 1; }
echo "all desktop-version tests passed"
