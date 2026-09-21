#!/bin/sh
# install-app_test.sh — drives scripts/install-app.sh against a signed fixture
# site and a fixture daemon.
#
# The installer decides which release to install by asking the daemon, and then
# installs code into ~/Applications. Everything it must get right is a refusal:
# a document signed by another key or served without a signature, a release with
# no desktop component or no asset for this machine, a download that does not
# hash to what the signed document says, an archive that writes outside the
# directory it is unpacked into, a release label from the daemon that is not a
# label, and a daemon that cannot be reached at all. Each of those must leave
# ~/Applications exactly as it found it, and a bundle already installed there
# must survive.
#
# The Keychain and the daemon are fixtures: the real login Keychain is never
# read, and no test needs an entry in it.
#
# Usage: sh scripts/install-app_test.sh
set -eu

HERE=$(cd "$(dirname "$0")" && pwd)
INSTALL_SH="$HERE/install-app.sh"
KEYS_GO="$HERE/../source/daemon/internal/platform/release/keys.go"

command -v python3 >/dev/null 2>&1 || { echo "SKIP: python3 is needed to serve the fixture" >&2; exit 0; }
command -v openssl >/dev/null 2>&1 || { echo "SKIP: openssl is needed to sign the fixture" >&2; exit 0; }

WORK=$(mktemp -d "${TMPDIR:-/tmp}/tumika-install-app-test.XXXXXX")
SITE_PID=""
DAEMON_PID=""
cleanup() {
  [ -n "$SITE_PID" ] && kill "$SITE_PID" 2>/dev/null
  [ -n "$DAEMON_PID" ] && kill "$DAEMON_PID" 2>/dev/null
  rm -rf "$WORK"
}
trap cleanup EXIT INT TERM

# The suite decides the platform it tests, so it runs the same way on a machine
# that is not the one the installer supports. Both seams are honoured only
# against a non-default TUMIKA_BASE_URL, which every run below sets.
RAW_ARCH=$(uname -m)
case "$RAW_ARCH" in
  x86_64 | amd64) ARCH=amd64 ;;
  arm64 | aarch64) ARCH=arm64 ;;
  *) echo "SKIP: no desktop asset key for $RAW_ARCH" >&2; exit 0 ;;
esac
PLATFORM="darwin_${ARCH}"

FAILURES=0
ok()   { echo "  ok: $*"; }
fail() { echo "  FAIL: $*" >&2; FAILURES=$((FAILURES + 1)); }

sha256() {
  if command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{ print $1 }'
  else
    sha256sum "$1" | awk '{ print $1 }'
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

# The updater archives. Each holds text naming the component version, so an
# installed bundle says which release it came from.
cat >"$WORK/mkapp.py" <<'PY'
"""Writes one .app.tar.gz fixture: mkapp.py <out> <kind> <component version>."""
import io
import sys
import tarfile


def member(archive, name, text):
    data = text.encode()
    info = tarfile.TarInfo(name)
    info.size = len(data)
    info.mode = 0o755
    archive.addfile(info, io.BytesIO(data))


out, kind, version = sys.argv[1], sys.argv[2], sys.argv[3]
with tarfile.open(out, "w:gz") as archive:
    if kind == "good":
        member(archive, "Tumika.app/Contents/MacOS/tumika", f"Tumika {version}\n")
        member(archive, "Tumika.app/Contents/Info.plist", f"<plist>{version}</plist>\n")
    elif kind == "escape":
        member(archive, "Tumika.app/Contents/MacOS/tumika", f"Tumika {version}\n")
        member(archive, "../escaped", "outside\n")
    elif kind == "wrong-top":
        member(archive, "Other.app/Contents/MacOS/tumika", f"Tumika {version}\n")
    else:
        raise SystemExit(f"unknown kind {kind}")
PY

# asset <label> <version> [kind] writes the updater archive a release publishes
# and prints its digest.
asset() {
  dir="$WORK/serve/download/$1"
  mkdir -p "$dir"
  python3 "$WORK/mkapp.py" "$dir/tumika-desktop_$2_$PLATFORM.app.tar.gz" "${3:-good}" "$2"
  sha256 "$dir/tumika-desktop_$2_$PLATFORM.app.tar.gz"
}

# bom <path> <label> <channel> <version> <url> <sha256> [asset platform] writes
# one bill of materials in the shape tumika-bom publishes — two-space
# indentation, one key per line — and signs it with the fixture release key. The
# desktop asset carries the `signature` field the app's updater reads and this
# installer ignores.
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
      "version": "0.0.1",
      "assets": {
        "$PLATFORM": {
          "url": "$BASE_URL/download/$2/tumika_0.0.1_$PLATFORM",
          "sha256": "0000000000000000000000000000000000000000000000000000000000000000"
        }
      }
    },
    "desktop": {
      "version": "$4",
      "assets": {
        "${7:-$PLATFORM}": {
          "url": "$5",
          "sha256": "$6",
          "signature": "dW50cnVzdGVkIGNvbW1lbnQ6IGZpeHR1cmUK"
        }
      }
    }
  }
}
EOF
  sign "$path" "$WORK/release.key"
}

# bom_without_desktop <path> <label> writes a release that ships the daemon
# alone, which is what a release carrying no desktop change looks like to a
# reader that only knows one component.
bom_without_desktop() {
  path="$WORK/serve/$1"
  mkdir -p "$(dirname "$path")"
  cat >"$path" <<EOF
{
  "release": "$2",
  "channel": "stable",
  "published_at": "2026-09-20T07:28:00Z",
  "components": {
    "daemon": {
      "version": "0.0.1",
      "assets": {
        "$PLATFORM": {
          "url": "$BASE_URL/download/$2/tumika_0.0.1_$PLATFORM",
          "sha256": "0000000000000000000000000000000000000000000000000000000000000000"
        }
      }
    }
  }
}
EOF
  sign "$path" "$WORK/release.key"
}

# release <label> <version> [archive kind] publishes one release: its updater
# archive and the signed document naming it.
release() {
  sum=$(asset "$1" "$2" "${3:-good}")
  bom "releases/$1.json" "$1" stable "$2" \
    "$BASE_URL/download/$1/tumika-desktop_$2_$PLATFORM.app.tar.gz" "$sum"
}

# The fixture daemon answers /v1/version behind the bearer token, and reads both
# the token it expects and the release it reports off disk, so a case changes
# either without restarting it.
cat >"$WORK/daemon.py" <<'PY'
"""A stand-in for the daemon's authenticated version endpoint."""
import json
import pathlib
import socketserver
import sys
from http.server import BaseHTTPRequestHandler

BASE = pathlib.Path(sys.argv[1])


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):  # noqa: N802 - the name BaseHTTPRequestHandler dispatches to
        if self.path != "/v1/version":
            self.send_error(404)
            return
        want = "Bearer " + (BASE / "daemon-token").read_text().strip()
        if self.headers.get("Authorization") != want:
            self.send_error(401)
            return
        body = json.dumps(
            {
                "version": "0.0.1",
                "release": (BASE / "daemon-release").read_text().strip(),
                "commit": "fixture",
                "channel": "stable",
                "schema_version": 3,
            }
        ).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass


server = socketserver.TCPServer(("127.0.0.1", 0), Handler)
print(server.server_address[1], flush=True)
server.serve_forever()
PY

# The Keychain stubs. Each stands in for /usr/bin/security: the token arrives on
# stdout, as it does from the real one.
mkdir -p "$WORK/stub"
cat >"$WORK/stub/security-ok" <<EOF
#!/bin/sh
cat "$WORK/daemon-token"
EOF
cat >"$WORK/stub/security-base64" <<EOF
#!/bin/sh
printf 'go-keyring-base64:%s\n' "\$(openssl base64 -A -in "$WORK/daemon-token")"
EOF
cat >"$WORK/stub/security-missing" <<'EOF'
#!/bin/sh
echo "security: SecKeychainSearchCopyNext: The specified item could not be found." >&2
exit 44
EOF
cat >"$WORK/stub/security-hostile" <<'EOF'
#!/bin/sh
printf 'tmk_has a space\n'
EOF
chmod 0755 "$WORK/stub"/security-*

# port_of <log> waits for a fixture server to report the port the kernel gave
# it. Port 0 is what keeps concurrent runs from colliding.
port_of() {
  i=0
  while [ "$i" -lt 50 ]; do
    PORT=$(sed -n 's/^\([0-9][0-9]*\)$/\1/p;s/.*port \([0-9]*\).*/\1/p' "$1" | head -1)
    [ -n "$PORT" ] && return 0
    i=$((i + 1))
    sleep 0.1
  done
  cat "$1" >&2
  echo "FAIL: a fixture server never reported a port" >&2
  exit 1
}

mkdir -p "$WORK/serve"
# -u because the port is read back out of the redirected stdout, which is
# block-buffered otherwise and stays empty until the server exits.
python3 -u -m http.server 0 --bind 127.0.0.1 --directory "$WORK/serve" >"$WORK/site.log" 2>&1 &
SITE_PID=$!
port_of "$WORK/site.log"
BASE_URL="http://127.0.0.1:$PORT"

printf 'tmk_fixture_token\n' >"$WORK/daemon-token"
printf '2026.09.01\n' >"$WORK/daemon-release"
python3 -u "$WORK/daemon.py" "$WORK" >"$WORK/daemon.log" 2>&1 &
DAEMON_PID=$!
port_of "$WORK/daemon.log"
DAEMON_URL="http://127.0.0.1:$PORT"

# Nothing listens here, which is what an install run before the daemon exists
# meets.
DEAD_PORT=$(python3 -c 'import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()')

echo "==> fixture site on $BASE_URL, fixture daemon on $DAEMON_URL"

# run [VAR=VALUE ...] installs into a fresh HOME against the fixture site, with
# the fixture key as the trusted one. Assignments passed in override the
# defaults, because env applies them in order. Sets $STATUS, $OUTPUT and $HOME_DIR.
SEQ=0
run() {
  HOME_DIR="$WORK/home.$SEQ"
  SEQ=$((SEQ + 1))
  mkdir -p "$HOME_DIR"
  set +e
  OUTPUT=$(env HOME="$HOME_DIR" TUMIKA_BASE_URL="$BASE_URL" \
    TUMIKA_TRUSTED_KEY_FILE="$WORK/release.pub" TUMIKA_DAEMON_URL="$DAEMON_URL" \
    TUMIKA_SECURITY_BIN="$WORK/stub/security-ok" \
    TUMIKA_UNAME_S=Darwin TUMIKA_UNAME_M="$RAW_ARCH" \
    "$@" sh "$INSTALL_SH" 2>&1)
  STATUS=$?
  set -e
}

# refused <what> <message> asserts the run failed, said <message>, and installed
# no bundle.
refused() {
  [ "$STATUS" -ne 0 ] || fail "$1: installed something"
  echo "$OUTPUT" | grep -q "$2" || fail "$1: the error does not say why: $OUTPUT"
  [ ! -e "$HOME_DIR/Applications/Tumika.app" ] || fail "$1: a bundle was installed anyway"
  [ "$STATUS" -eq 0 ] || ok "$(echo "$OUTPUT" | grep '^error:' | head -1)"
}

# installed <what> <component version> asserts the bundle in ~/Applications is
# the one that release publishes, and that nothing was left staged beside it.
installed() {
  if [ "$STATUS" -ne 0 ]; then
    echo "$OUTPUT" | sed 's/^/    /' >&2
    fail "$1: the install returned $STATUS"
    return
  fi
  if ! grep -q "Tumika $2" "$HOME_DIR/Applications/Tumika.app/Contents/MacOS/tumika" 2>/dev/null; then
    fail "$1: ~/Applications/Tumika.app is not Tumika $2"
    return
  fi
  leftovers=$(find "$HOME_DIR/Applications" -maxdepth 1 -name '.tumika-install.*' | head -1)
  [ -z "$leftovers" ] || fail "$1: the staging directory was left behind"
  ok "$1"
}

# The release the fixture daemon reports, and one to pin by label.
release 2026.09.01 0.2.0
release 2026.09.02 0.3.0

# Failure fixtures, each addressed by a label of its own so one site serves them
# all.
sum=$(asset 2026.09.03 0.2.0)
bom "releases/2026.09.03.json" 2026.09.03 stable 0.2.0 \
  "$BASE_URL/download/2026.09.03/tumika-desktop_0.2.0_$PLATFORM.app.tar.gz" \
  0000000000000000000000000000000000000000000000000000000000000000

sum=$(asset 2026.09.04 0.2.0)
bom "releases/2026.09.04.json" 2026.09.04 stable 0.2.0 \
  "$BASE_URL/download/2026.09.04/tumika-desktop_0.2.0_$PLATFORM.app.tar.gz" "$sum"
sign "$WORK/serve/releases/2026.09.04.json" "$WORK/other.key"

sum=$(asset 2026.09.05 0.2.0)
bom "releases/2026.09.05.json" 2026.09.05 stable 0.2.0 \
  "$BASE_URL/download/2026.09.05/tumika-desktop_0.2.0_$PLATFORM.app.tar.gz" "$sum"
rm "$WORK/serve/releases/2026.09.05.json.sig"

bom_without_desktop "releases/2026.09.06.json" 2026.09.06

sum=$(asset 2026.09.07 0.2.0)
bom "releases/2026.09.07.json" 2026.09.07 stable 0.2.0 \
  "$BASE_URL/download/2026.09.07/tumika-desktop_0.2.0_$PLATFORM.app.tar.gz" "$sum" darwin_riscv64

release 2026.09.08 0.2.0 escape
release 2026.09.09 0.2.0 wrong-top

sum=$(asset 2026.09.10 0.2.0)
bom "releases/2026.09.10.json" 2026.09.99 stable 0.2.0 \
  "$BASE_URL/download/2026.09.10/tumika-desktop_0.2.0_$PLATFORM.app.tar.gz" "$sum"

echo "==> the daemon's release decides which app is installed"
run
installed "installed the release the daemon runs" 0.2.0
if [ "$STATUS" -eq 0 ]; then
  echo "$OUTPUT" | grep -q "the daemon runs release 2026.09.01 on the stable channel" \
    || fail "the run does not report what the daemon answered: $OUTPUT"
  echo "$OUTPUT" | grep -q "verified the signature on $BASE_URL/releases/2026.09.01.json" \
    || fail "the run does not report verifying the release document"
  echo "$OUTPUT" | grep -q "installed Tumika 0.2.0 from release 2026.09.01 to $HOME_DIR/Applications/Tumika.app" \
    || fail "the run does not say what was installed where: $OUTPUT"
  echo "$OUTPUT" | grep -q "quit it and open it again" \
    || fail "the run does not say a running app keeps its own component version"
  # The token reaches curl on stdin and nowhere else, so no line of a run —
  # including the ones reporting a failure — may carry it.
  if echo "$OUTPUT" | grep -q 'tmk_fixture_token'; then fail "the API token is in the output"; fi
fi

echo "==> a token stored base64-encoded is decoded before it is sent"
run TUMIKA_SECURITY_BIN="$WORK/stub/security-base64"
installed "a go-keyring-encoded token reaches the daemon" 0.2.0

echo "==> TUMIKA_RELEASE installs that release without asking any daemon"
run TUMIKA_RELEASE=2026.09.02 TUMIKA_DAEMON_URL="http://127.0.0.1:$DEAD_PORT" \
  TUMIKA_SECURITY_BIN="$WORK/stub/security-missing"
installed "a pinned label needs neither daemon nor Keychain" 0.3.0

echo "==> the leading v of a release tag is accepted"
run TUMIKA_RELEASE=v2026.09.02
installed "v2026.09.02 resolved to release 2026.09.02" 0.3.0

echo "==> an app already installed is replaced"
run TUMIKA_RELEASE=2026.09.02
PREVIOUS="$HOME_DIR"
if [ "$STATUS" -eq 0 ]; then
  set +e
  OUTPUT=$(env HOME="$PREVIOUS" TUMIKA_BASE_URL="$BASE_URL" \
    TUMIKA_TRUSTED_KEY_FILE="$WORK/release.pub" TUMIKA_RELEASE=2026.09.01 \
    TUMIKA_UNAME_S=Darwin TUMIKA_UNAME_M="$RAW_ARCH" sh "$INSTALL_SH" 2>&1)
  STATUS=$?
  set -e
  HOME_DIR="$PREVIOUS"
  installed "the bundle in place was replaced" 0.2.0
fi

echo "==> a download that does not match the signed digest leaves the installed app alone"
run TUMIKA_RELEASE=2026.09.02
PREVIOUS="$HOME_DIR"
if [ "$STATUS" -eq 0 ]; then
  set +e
  OUTPUT=$(env HOME="$PREVIOUS" TUMIKA_BASE_URL="$BASE_URL" \
    TUMIKA_TRUSTED_KEY_FILE="$WORK/release.pub" TUMIKA_RELEASE=2026.09.03 \
    TUMIKA_UNAME_S=Darwin TUMIKA_UNAME_M="$RAW_ARCH" sh "$INSTALL_SH" 2>&1)
  STATUS=$?
  set -e
  HOME_DIR="$PREVIOUS"
  [ "$STATUS" -ne 0 ] || fail "a checksum mismatch installed something"
  echo "$OUTPUT" | grep -q "checksum mismatch" || fail "a checksum mismatch does not say so: $OUTPUT"
  grep -q "Tumika 0.3.0" "$HOME_DIR/Applications/Tumika.app/Contents/MacOS/tumika" \
    || fail "a refused install did not leave the bundle in place"
  [ "$STATUS" -eq 0 ] || ok "$(echo "$OUTPUT" | grep '^error:' | head -1)"
fi

echo "==> macOS is the only platform the app is built for"
run TUMIKA_UNAME_S=Linux
refused "a linux host" "macOS-only"
echo "$OUTPUT" | grep -q "install-daemon.sh" || fail "the linux refusal does not point at the daemon installer"

echo "==> an architecture no asset is keyed by is refused"
run TUMIKA_UNAME_M=riscv64
refused "an unsupported architecture" "unsupported architecture: riscv64"

echo "==> a document signed by another key is refused"
run TUMIKA_RELEASE=2026.09.04
refused "another signer" "does not verify against the tumika release key"

echo "==> a document served without its signature is refused"
run TUMIKA_RELEASE=2026.09.05
refused "an unsigned document" "is unsigned"

echo "==> a release that ships no desktop component is refused"
run TUMIKA_RELEASE=2026.09.06
refused "no desktop component" "ships no desktop component"

echo "==> a release with no desktop asset for this machine is refused"
run TUMIKA_RELEASE=2026.09.07
refused "no asset for this platform" "publishes no desktop asset for $PLATFORM"

echo "==> an archive that writes outside its directory is refused"
run TUMIKA_RELEASE=2026.09.08
refused "an escaping archive" "escapes its directory"

echo "==> an archive that is not a Tumika.app bundle is refused"
run TUMIKA_RELEASE=2026.09.09
refused "an archive holding something else" "it holds Tumika.app and nothing else"

echo "==> a document describing another release is refused"
run TUMIKA_RELEASE=2026.09.10
refused "the wrong release" "describes release 2026.09.99, not 2026.09.10"

echo "==> a label that is not a release label never reaches a URL"
run TUMIKA_RELEASE=../../etc/passwd
refused "a label outside the shape" "is not a tumika release label"

echo "==> a release label the daemon reports is held to the same shape"
printf '../../etc/passwd\n' >"$WORK/daemon-release"
run
refused "a hostile label from the daemon" "which is not a release label"
printf 'dev\n' >"$WORK/daemon-release"
run
refused "a daemon built from no release" "which is not a release label"
printf '2026.09.01\n' >"$WORK/daemon-release"

echo "==> a daemon that cannot be reached stops the install"
run TUMIKA_DAEMON_URL="http://127.0.0.1:$DEAD_PORT"
refused "no daemon listening" "could not be read"
echo "$OUTPUT" | grep -q "install-daemon.sh" || fail "the unreachable-daemon refusal does not say how to fix it"

echo "==> a login Keychain with no tumika token stops the install"
run TUMIKA_SECURITY_BIN="$WORK/stub/security-missing"
refused "no token in the keychain" "holds no tumika API token"

echo "==> a token that is not in the shape tumika issues never reaches a header"
run TUMIKA_SECURITY_BIN="$WORK/stub/security-hostile"
refused "a hostile token" "not in the shape tumika issues"

echo "==> a token the daemon does not accept is reported, not worked around"
printf 'tmk_other_token\n' >"$WORK/daemon-token.other"
cat >"$WORK/stub/security-other" <<EOF
#!/bin/sh
cat "$WORK/daemon-token.other"
EOF
chmod 0755 "$WORK/stub/security-other"
run TUMIKA_SECURITY_BIN="$WORK/stub/security-other"
refused "a token the daemon rejects" "could not be read"

echo "==> the test seams are refused against the real site"
for seam in TUMIKA_TRUSTED_KEY_FILE=/dev/null TUMIKA_DAEMON_URL=http://example.invalid \
  TUMIKA_SECURITY_BIN=/bin/echo TUMIKA_UNAME_S=Linux TUMIKA_UNAME_M=riscv64; do
  HOME_DIR="$WORK/home.seam.${seam%%=*}"
  mkdir -p "$HOME_DIR"
  set +e
  OUTPUT=$(env HOME="$HOME_DIR" "$seam" sh "$INSTALL_SH" 2>&1)
  STATUS=$?
  set -e
  refused "${seam%%=*} against the default base url" "only honoured with TUMIKA_BASE_URL pointing elsewhere"
done

echo "==> the embedded release key is the daemon's first release key"
# scripts/install-app.sh embeds the key because it runs before any tumika binary
# exists and has nothing else to trust. The two drifting is silent in both
# directions: a rotated compiled-in list leaves every new install verifying
# against a key the publisher no longer uses, and a rotated script leaves the
# app installed from a release the daemon will not follow.
first_pem() {
  awk '/-----BEGIN PUBLIC KEY-----/ { p = 1 } p { print } /-----END PUBLIC KEY-----/ { if (p) exit }' "$1" |
    sed 's/^[^-]*-----BEGIN/-----BEGIN/; s/-----END PUBLIC KEY-----.*/-----END PUBLIC KEY-----/'
}
if [ ! -r "$KEYS_GO" ]; then
  fail "keys.go is not readable at $KEYS_GO; it is the list a release is signed against"
elif [ "$(first_pem "$INSTALL_SH")" != "$(first_pem "$KEYS_GO")" ]; then
  fail "the key install-app.sh embeds is not releaseKeyPEMs[0] in keys.go"
else
  ok "install-app.sh embeds releaseKeyPEMs[0]"
fi

echo
if [ "$FAILURES" -ne 0 ]; then
  echo "FAIL ($FAILURES)"
  exit 1
fi
echo "PASS"
