#!/bin/sh
# install_test.sh — drives scripts/install.sh against a local release fixture.
#
# install.sh is fetched by `curl | sh` and is the only path a first-time user
# has, so the things it must get right are exactly the ones nothing else
# exercises: that the asset name comes from the release's release.yaml rather
# than from the tag, and that every way of failing to establish that name stops
# before anything is installed.
#
# Usage: sh scripts/install_test.sh
set -eu

HERE=$(cd "$(dirname "$0")" && pwd)
INSTALL_SH="$HERE/install.sh"

command -v python3 >/dev/null 2>&1 || { echo "SKIP: python3 is needed to serve the fixture" >&2; exit 0; }

WORK=$(mktemp -d "${TMPDIR:-/tmp}/tumika-install-test.XXXXXX")
SERVER_PID=""
cleanup() {
  [ -n "$SERVER_PID" ] && kill "$SERVER_PID" 2>/dev/null
  rm -rf "$WORK"
}
trap cleanup EXIT INT TERM

OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)
case "$ARCH" in
  x86_64 | amd64) ARCH=amd64 ;;
  aarch64 | arm64) ARCH=arm64 ;;
esac

FAILURES=0
ok()   { echo "  ok: $*"; }
fail() { echo "  FAIL: $*" >&2; FAILURES=$((FAILURES + 1)); }

sha256() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{ print $1 }'
  else
    shasum -a 256 "$1" | awk '{ print $1 }'
  fi
}

# Lays out one release under the served root: the raw binary asset named after
# the component version, a checksums.txt covering it, and release.yaml.
# Usage: fixture <tag> <component-version> [checksum-override]
fixture() {
  tag="$1"; component="$2"; override="${3:-}"
  dir="$WORK/serve/releases/download/$tag"
  mkdir -p "$dir"
  asset="tumika_${component}_${OS}_${ARCH}"
  printf '#!/bin/sh\necho tumika %s\n' "$component" >"$dir/$asset"
  sum="${override:-$(sha256 "$dir/$asset")}"
  printf '%s  %s\n' "$sum" "$asset" >"$dir/checksums.txt"
  printf 'release: %s\ncomponents:\n  daemon: %s\n' "${tag#v}" "$component" >"$dir/release.yaml"
}

# Runs install.sh against the fixture server into a fresh install dir, capturing
# stdout and stderr together. Usage: run <tag> ; then $STATUS, $OUTPUT, $DIR.
run() {
  DIR="$WORK/bin.$(date +%s).$$.$RANDOM_SEQ"
  RANDOM_SEQ=$((RANDOM_SEQ + 1))
  mkdir -p "$DIR"
  set +e
  OUTPUT=$(TUMIKA_VERSION="$1" TUMIKA_INSTALL_DIR="$DIR" TUMIKA_RELEASES_URL="$BASE_URL" \
    sh "$INSTALL_SH" 2>&1)
  STATUS=$?
  set -e
}
RANDOM_SEQ=0

mkdir -p "$WORK/serve"
fixture v2026.09.01 0.0.1
fixture v2026.09.02-beta.1 0.0.1-beta.1
fixture v2026.09.03 0.0.1            # release.yaml removed below
rm "$WORK/serve/releases/download/v2026.09.03/release.yaml"
fixture v2026.09.04 0.0.1
printf 'release: 2026.09.04\ncomponents:\n  desktop: 0.1.0\n' \
  >"$WORK/serve/releases/download/v2026.09.04/release.yaml"
fixture v2026.09.05 0.0.1 0000000000000000000000000000000000000000000000000000000000000000
fixture v2026.09.06 0.0.1
printf 'release: 2026.09.06\ncomponents:\n  daemon: ../../etc/passwd\n' \
  >"$WORK/serve/releases/download/v2026.09.06/release.yaml"

# -u because the port is read back out of the redirected stdout, which is
# block-buffered otherwise and stays empty until the server exits. Port 0 asks
# the kernel for a free one, so concurrent runs never collide.
python3 -u -m http.server 0 --bind 127.0.0.1 --directory "$WORK/serve" >"$WORK/http.log" 2>&1 &
SERVER_PID=$!
PORT=""
i=0
while [ "$i" -lt 50 ]; do
  PORT=$(sed -n 's/.*port \([0-9]*\).*/\1/p' "$WORK/http.log" | head -1)
  [ -n "$PORT" ] && break
  i=$((i + 1))
  sleep 0.1
done
[ -n "$PORT" ] || { cat "$WORK/http.log" >&2; echo "FAIL: the fixture server never reported a port" >&2; exit 1; }
BASE_URL="http://127.0.0.1:$PORT"
echo "==> fixture server on $BASE_URL"

echo "==> a calendar tag installs the component-version asset"
run v2026.09.01
if [ "$STATUS" -ne 0 ]; then
  echo "$OUTPUT" | sed 's/^/    /' >&2
  fail "install returned $STATUS"
else
  echo "$OUTPUT" | sed 's/^/    /'
  echo "$OUTPUT" | grep -q "downloading tumika 0.0.1 from release v2026.09.01 (${OS}/${ARCH})" \
    || fail "the download message names neither the component version nor the release tag"
  grep -q 'echo tumika 0.0.1' "$DIR/tumika" || fail "the installed binary is not the 0.0.1 asset"
  [ -x "$DIR/tumika" ] || fail "the installed binary is not executable"
  ok "installed tumika_0.0.1_${OS}_${ARCH}"
fi

echo "==> the tag is accepted without its leading v"
run 2026.09.01
[ "$STATUS" -eq 0 ] || fail "a tag without the leading v returned $STATUS"
[ "$STATUS" -ne 0 ] || ok "2026.09.01 resolved to v2026.09.01"

echo "==> a beta tag installs a prerelease component version"
run v2026.09.02-beta.1
if [ "$STATUS" -ne 0 ]; then
  echo "$OUTPUT" | sed 's/^/    /' >&2
  fail "the beta install returned $STATUS"
else
  echo "$OUTPUT" | grep -q "downloading tumika 0.0.1-beta.1 from release v2026.09.02-beta.1" \
    || fail "the beta download message is wrong: $OUTPUT"
  grep -q 'echo tumika 0.0.1-beta.1' "$DIR/tumika" || fail "the installed binary is not the beta asset"
  ok "installed tumika_0.0.1-beta.1_${OS}_${ARCH}"
fi

echo "==> a release without release.yaml fails with the reason"
run v2026.09.03
[ "$STATUS" -ne 0 ] || fail "a release with no release.yaml installed something"
echo "$OUTPUT" | grep -q 'no release.yaml asset' || fail "the error does not name the missing asset: $OUTPUT"
[ ! -e "$DIR/tumika" ] || fail "a binary was installed anyway"
ok "$(echo "$OUTPUT" | grep '^error:' | head -1)"

echo "==> a release.yaml with no daemon entry fails with the reason"
run v2026.09.04
[ "$STATUS" -ne 0 ] || fail "a release.yaml with no daemon entry installed something"
echo "$OUTPUT" | grep -q 'names no daemon version' || fail "the error does not name the missing entry: $OUTPUT"
[ ! -e "$DIR/tumika" ] || fail "a binary was installed anyway"
ok "$(echo "$OUTPUT" | grep '^error:' | head -1)"

echo "==> a checksum mismatch refuses to install"
run v2026.09.05
[ "$STATUS" -ne 0 ] || fail "a mismatched checksum installed something"
echo "$OUTPUT" | grep -q 'checksum mismatch' || fail "the error is not a checksum mismatch: $OUTPUT"
[ ! -e "$DIR/tumika" ] || fail "a binary was installed anyway"
ok "$(echo "$OUTPUT" | grep '^error:' | head -1)"

echo "==> a daemon version that is not semver never reaches a path or a URL"
run v2026.09.06
[ "$STATUS" -ne 0 ] || fail "a non-semver daemon version installed something"
echo "$OUTPUT" | grep -q 'is not semver' || fail "the error does not name the bad version: $OUTPUT"
[ ! -e "$DIR/tumika" ] || fail "a binary was installed anyway"
ok "$(echo "$OUTPUT" | grep '^error:' | head -1)"

echo
if [ "$FAILURES" -ne 0 ]; then
  echo "FAIL ($FAILURES)"
  exit 1
fi
echo "PASS"
