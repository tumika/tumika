#!/bin/sh
# install-daemon_test.sh — drives scripts/install-daemon.sh against a signed
# fixture site.
#
# install-daemon.sh is fetched by `curl | sh` and is the only path a first-time
# user has. Everything it must get right is a refusal: a document signed by
# another key, a document edited after signing, a document served without its
# signature, one that answers a different question than the one asked, and a
# download that does not hash to what the signed document says. Each of those
# must leave the install directory untouched.
#
# Usage: sh scripts/install-daemon_test.sh
set -eu

HERE=$(cd "$(dirname "$0")" && pwd)
INSTALL_SH="$HERE/install-daemon.sh"

command -v python3 >/dev/null 2>&1 || { echo "SKIP: python3 is needed to serve the fixture" >&2; exit 0; }
command -v openssl >/dev/null 2>&1 || { echo "SKIP: openssl is needed to sign the fixture" >&2; exit 0; }

WORK=$(mktemp -d "${TMPDIR:-/tmp}/tumika-install-daemon-test.XXXXXX")
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
PLATFORM="${OS}_${ARCH}"

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

# The fixture signing key stands in for the release key the script embeds; the
# other key is a signer the script must not trust. Both are P-256, because that
# is the only curve the format has.
openssl ecparam -name prime256v1 -genkey -noout -out "$WORK/release.key" 2>/dev/null
openssl ec -in "$WORK/release.key" -pubout -out "$WORK/release.pub" 2>/dev/null
openssl ecparam -name prime256v1 -genkey -noout -out "$WORK/other.key" 2>/dev/null

# sign <document> <private-key> writes the detached signature beside it, in the
# encoding release.Sign produces: base64 of the ASN.1 DER signature, one line.
sign() {
  openssl dgst -sha256 -sign "$2" -out "$WORK/sig.der" "$1"
  { openssl base64 -A -in "$WORK/sig.der"; echo; } >"$1.sig"
}

# asset <label> <version> writes the raw binary a release publishes and prints
# its digest.
asset() {
  dir="$WORK/serve/download/$1"
  mkdir -p "$dir"
  printf '#!/bin/sh\necho tumika %s\n' "$2" >"$dir/tumika_$2_$PLATFORM"
  sha256 "$dir/tumika_$2_$PLATFORM"
}

# bom <path> <label> <channel> <version> <url> <sha256> writes one bill of
# materials in the shape tumika-bom publishes — two-space indentation, one key
# per line — and signs it with the fixture release key.
bom() {
  path="$WORK/serve/$1"
  mkdir -p "$(dirname "$path")"
  cat >"$path" <<EOF
{
  "release": "$2",
  "channel": "$3",
  "published_at": "2026-09-20T07:28:00Z",
  "components": {
    "daemon": {
      "version": "$4",
      "assets": {
        "$PLATFORM": {
          "url": "$5",
          "sha256": "$6"
        },
        "plan9_sparc": {
          "url": "$BASE_URL/download/nowhere/tumika_$4_plan9_sparc",
          "sha256": "0000000000000000000000000000000000000000000000000000000000000000"
        }
      }
    },
    "desktop": {
      "version": "0.1.0",
      "assets": {
        "$PLATFORM": {
          "url": "$BASE_URL/download/nowhere/tumika-desktop_0.1.0_$PLATFORM.tar.gz",
          "sha256": "1111111111111111111111111111111111111111111111111111111111111111"
        }
      },
      "from_release": "2026.09.00"
    }
  }
}
EOF
  sign "$path" "$WORK/release.key"
}

# release <label> <channel> <version> publishes one release: its raw binary and
# the signed document naming it.
release() {
  sum=$(asset "$1" "$3")
  bom "releases/$1.json" "$1" "$2" "$3" "$BASE_URL/download/$1/tumika_$3_$PLATFORM" "$sum"
}

# channel_head <channel> <label> <version> republishes a release as a channel's
# head. The head declares the channel it is SERVED under, which is what
# tumika-bom writes and what the script checks it against.
channel_head() {
  sum=$(sha256 "$WORK/serve/download/$2/tumika_$3_$PLATFORM")
  bom "channels/$1.json" "$2" "$1" "$3" "$BASE_URL/download/$2/tumika_$3_$PLATFORM" "$sum"
}

# run [VAR=VALUE ...] installs into a fresh directory against the fixture site,
# with the fixture key as the trusted one. Sets $STATUS, $OUTPUT and $DIR.
run() {
  DIR="$WORK/bin.$RANDOM_SEQ"
  RANDOM_SEQ=$((RANDOM_SEQ + 1))
  mkdir -p "$DIR"
  set +e
  OUTPUT=$(env "$@" TUMIKA_INSTALL_DIR="$DIR" TUMIKA_BASE_URL="$BASE_URL" \
    TUMIKA_TRUSTED_KEY_FILE="$WORK/release.pub" sh "$INSTALL_SH" 2>&1)
  STATUS=$?
  set -e
}
RANDOM_SEQ=0

# refused <what> asserts the run failed, said <message> and left the install
# directory exactly as it found it.
refused() {
  [ "$STATUS" -ne 0 ] || fail "$1: installed something"
  echo "$OUTPUT" | grep -q "$2" || fail "$1: the error does not say why: $OUTPUT"
  [ -z "$(find "$DIR" -mindepth 1)" ] || fail "$1: the install directory is not empty"
  [ "$STATUS" -eq 0 ] || ok "$(echo "$OUTPUT" | grep '^error:' | head -1)"
}

mkdir -p "$WORK/serve"
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
echo "==> fixture site on $BASE_URL"

# The three channel heads, each pointing at a release of its own so the
# installed binary names the channel it came from.
release 2026.09.01 stable 0.0.1
release 2026.09.02-beta.1 beta 0.0.2-beta.1
release edge.417 edge 0.0.3-edge.417
channel_head stable 2026.09.01 0.0.1
channel_head beta 2026.09.02-beta.1 0.0.2-beta.1
channel_head edge edge.417 0.0.3-edge.417

# Failure fixtures, each addressed by a label of its own so one server serves
# them all.
sum=$(asset 2026.09.11 0.0.1)
bom "releases/2026.09.11.json" 2026.09.11 stable 0.0.1 "$BASE_URL/download/2026.09.11/tumika_0.0.1_$PLATFORM" "$sum"
sign "$WORK/serve/releases/2026.09.11.json" "$WORK/other.key"

sum=$(asset 2026.09.12 0.0.1)
bom "releases/2026.09.12.json" 2026.09.12 stable 0.0.1 "$BASE_URL/download/2026.09.12/tumika_0.0.1_$PLATFORM" "$sum"
sed 's/"version": "0.0.1"/"version": "9.9.9"/' "$WORK/serve/releases/2026.09.12.json" >"$WORK/tampered"
mv "$WORK/tampered" "$WORK/serve/releases/2026.09.12.json"

sum=$(asset 2026.09.13 0.0.1)
bom "releases/2026.09.13.json" 2026.09.13 stable 0.0.1 "$BASE_URL/download/2026.09.13/tumika_0.0.1_$PLATFORM" "$sum"
sed "s/\"$PLATFORM\"/\"sunos_mips\"/" "$WORK/serve/releases/2026.09.13.json" >"$WORK/noasset"
mv "$WORK/noasset" "$WORK/serve/releases/2026.09.13.json"
sign "$WORK/serve/releases/2026.09.13.json" "$WORK/release.key"

asset 2026.09.14 0.0.1 >/dev/null
bom "releases/2026.09.14.json" 2026.09.14 stable 0.0.1 "$BASE_URL/download/2026.09.14/tumika_0.0.1_$PLATFORM" \
  0000000000000000000000000000000000000000000000000000000000000000

sum=$(asset 2026.09.15 0.0.1)
bom "releases/2026.09.15.json" 2026.09.15 stable 0.0.1 "$BASE_URL/download/2026.09.15/tumika_0.0.1_$PLATFORM" "$sum"
rm "$WORK/serve/releases/2026.09.15.json.sig"

sum=$(asset 2026.09.17 0.0.1)
bom "releases/2026.09.17.json" 2026.09.17 stable 0.0.1 "$BASE_URL/download/2026.09.17/tumika_0.0.1_$PLATFORM" "$sum"
printf '!!! not a signature !!!\n' >"$WORK/serve/releases/2026.09.17.json.sig"

sum=$(asset 2026.09.16 0.0.1)
bom "releases/2026.09.16.json" 2026.09.17 stable 0.0.1 "$BASE_URL/download/2026.09.16/tumika_0.0.1_$PLATFORM" "$sum"

echo "==> the stable channel installs the head's daemon asset"
run TUMIKA_CHANNEL=stable
if [ "$STATUS" -ne 0 ]; then
  echo "$OUTPUT" | sed 's/^/    /' >&2
  fail "the stable install returned $STATUS"
else
  echo "$OUTPUT" | sed 's/^/    /'
  echo "$OUTPUT" | grep -q "verified the signature on $BASE_URL/channels/stable.json" \
    || fail "the run does not report verifying the channel head"
  echo "$OUTPUT" | grep -q "downloading tumika 0.0.1 from release 2026.09.01 (${OS}/${ARCH})" \
    || fail "the download message names neither the component version nor the release"
  grep -q 'echo tumika 0.0.1' "$DIR/tumika" || fail "the installed binary is not the stable asset"
  [ -x "$DIR/tumika" ] || fail "the installed binary is not executable"
  ok "installed tumika_0.0.1_$PLATFORM from the stable head"
fi

echo "==> the beta channel installs a prerelease component version"
run TUMIKA_CHANNEL=beta
[ "$STATUS" -eq 0 ] || { echo "$OUTPUT" | sed 's/^/    /' >&2; fail "the beta install returned $STATUS"; }
if [ "$STATUS" -eq 0 ]; then
  grep -q 'echo tumika 0.0.2-beta.1' "$DIR/tumika" || fail "the installed binary is not the beta asset"
  ok "installed tumika_0.0.2-beta.1_$PLATFORM from the beta head"
fi

echo "==> the edge channel installs an edge component version"
run TUMIKA_CHANNEL=edge
[ "$STATUS" -eq 0 ] || { echo "$OUTPUT" | sed 's/^/    /' >&2; fail "the edge install returned $STATUS"; }
if [ "$STATUS" -eq 0 ]; then
  grep -q 'echo tumika 0.0.3-edge.417' "$DIR/tumika" || fail "the installed binary is not the edge asset"
  ok "installed tumika_0.0.3-edge.417_$PLATFORM from the edge head"
fi

echo "==> a pinned release label installs that release, whatever the channel says"
run TUMIKA_VERSION=2026.09.02-beta.1 TUMIKA_CHANNEL=stable
[ "$STATUS" -eq 0 ] || { echo "$OUTPUT" | sed 's/^/    /' >&2; fail "the pinned install returned $STATUS"; }
if [ "$STATUS" -eq 0 ]; then
  echo "$OUTPUT" | grep -q "$BASE_URL/releases/2026.09.02-beta.1.json" \
    || fail "a pinned label did not read the release document: $OUTPUT"
  grep -q 'echo tumika 0.0.2-beta.1' "$DIR/tumika" || fail "the pinned install is not release 2026.09.02-beta.1"
  ok "installed release 2026.09.02-beta.1 by label"
fi

echo "==> the leading v of a release tag is accepted"
run TUMIKA_VERSION=v2026.09.01
[ "$STATUS" -eq 0 ] || { echo "$OUTPUT" | sed 's/^/    /' >&2; fail "v2026.09.01 returned $STATUS"; }
[ "$STATUS" -ne 0 ] || ok "v2026.09.01 resolved to release 2026.09.01"

echo "==> a document signed by another key is refused"
run TUMIKA_VERSION=2026.09.11
refused "another signer" "does not verify against the tumika release key"

echo "==> a document edited after signing is refused"
run TUMIKA_VERSION=2026.09.12
refused "a tampered document" "does not verify against the tumika release key"

echo "==> a release with no asset for this platform is refused"
run TUMIKA_VERSION=2026.09.13
refused "no asset for this platform" "publishes no daemon asset for $PLATFORM"

echo "==> a download that does not match the signed digest is refused"
run TUMIKA_VERSION=2026.09.14
refused "a checksum mismatch" "checksum mismatch"

echo "==> a document served without its signature is refused"
run TUMIKA_VERSION=2026.09.15
refused "an unsigned document" "is unsigned"

echo "==> a signature that is not base64 is refused"
run TUMIKA_VERSION=2026.09.17
refused "an unreadable signature" "base64"

echo "==> a release document describing another release is refused"
run TUMIKA_VERSION=2026.09.16
refused "the wrong release" "describes release 2026.09.17, not 2026.09.16"

echo "==> a label nobody published is refused"
run TUMIKA_VERSION=2026.09.18
refused "an unpublished label" "no bill of materials at"

echo "==> a label that is not a release label never reaches a URL"
run TUMIKA_VERSION=../../etc/passwd
refused "a label outside the shape" "is not a tumika release label"

echo "==> a channel head declaring another channel is refused"
release 2026.09.19 stable 0.0.9
bom "channels/edge.json" 2026.09.19 stable 0.0.9 "$BASE_URL/download/2026.09.19/tumika_0.0.9_$PLATFORM" \
  "$(sha256 "$WORK/serve/download/2026.09.19/tumika_0.0.9_$PLATFORM")"
run TUMIKA_CHANNEL=edge
refused "a channel mismatch" "is the head of channel 'stable', not edge"

echo "==> a name that is not a channel never reaches a URL"
run TUMIKA_CHANNEL=nightly
refused "an unknown channel" "is not a tumika channel"

echo "==> the trusted-key seam is refused against the real site"
DIR="$WORK/bin.seam"
mkdir -p "$DIR"
set +e
OUTPUT=$(TUMIKA_INSTALL_DIR="$DIR" TUMIKA_TRUSTED_KEY_FILE="$WORK/release.pub" sh "$INSTALL_SH" 2>&1)
STATUS=$?
set -e
refused "the key seam with the default base url" "only honoured with TUMIKA_BASE_URL pointing elsewhere"

echo
if [ "$FAILURES" -ne 0 ]; then
  echo "FAIL ($FAILURES)"
  exit 1
fi
echo "PASS"
