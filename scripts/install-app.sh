#!/bin/sh
# install-app.sh — install the tumika desktop app the running daemon is paired with.
#
# Usage:
#   curl -fsSL https://get.tumika.org/install-app.sh | sh
#
# The app runs the desktop component version named by the bill of materials of
# the release its daemon runs, so the installer ASKS THE DAEMON which release to
# install: GET /v1/version, behind the bearer token the daemon left in the login
# Keychain. It then reads that release's document off the site, verifies the
# detached signature against the release public key embedded below, and only
# then reads the desktop asset's URL and SHA-256 out of it. Nothing the document
# says reaches a URL, a path or the disk before it verifies.
#
# There is no fallback to "whatever stable offers": installing a component
# version the local daemon is not paired with produces an app that immediately
# updates itself to something else, or refuses to, with no way to tell which.
# When the daemon cannot be reached, the script says so and stops.
#
# Environment:
#   TUMIKA_RELEASE      a release LABEL to install — 2026.09.01, 2026.09.01-beta.1
#                       or edge.417 (a leading v is accepted and dropped). It
#                       replaces the question to the daemon, so the app can be
#                       installed while no daemon is running. Nothing else is
#                       consulted, including the Keychain.
#   TUMIKA_BASE_URL     host the documents and the assets are read from
#                       (default: https://get.tumika.org). A TEST SEAM: it
#                       points the script at a fixture server.
#   TUMIKA_TRUSTED_KEY_FILE
#                       a PEM public key to verify the document with in place of
#                       the embedded one.
#   TUMIKA_DAEMON_URL   the daemon the release is read from (default:
#                       http://127.0.0.1:8737).
#   TUMIKA_SECURITY_BIN the binary the Keychain is read through (default:
#                       /usr/bin/security).
#   TUMIKA_UNAME_S, TUMIKA_UNAME_M
#                       the operating system and machine names to decide on.
#
# The last four, like TUMIKA_TRUSTED_KEY_FILE, are TEST SEAMS and are honoured
# ONLY when TUMIKA_BASE_URL names a host other than the default. An install
# reading the real site trusts the embedded key, the real Keychain and the real
# uname and nothing else, and whoever can point TUMIKA_BASE_URL elsewhere
# already chooses every byte the script sees.
#
# The app is installed into ~/Applications. Never /Applications, which needs an
# administrator and makes one user's install everybody's; never sudo.
#
# A running Tumika is NOT required to quit. macOS keeps a running app's open
# bundle files alive across the replacement, so the install succeeds and that
# app goes on running the component version it launched with until someone
# quits and opens it again. The script says so when it finishes.
set -eu

# Everything lives in main(), called on the LAST line.
#
# `curl … | sh` executes statements as they arrive, so a connection dropped
# mid-transfer runs whatever prefix landed. With a wrapper, a truncated download
# parses to a function definition that is never called, and does nothing at all.
main() {

# The release public key. It is the first entry of releaseKeyPEMs in
# source/daemon/internal/platform/release/keys.go, which is the list the daemon
# verifies its own updates against; the key case in install-app_test.sh fails
# when the two drift. Rotating the key means editing both.
RELEASE_PUBLIC_KEY='-----BEGIN PUBLIC KEY-----
MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEGgmfTHgWRmKZRGo7FrLeAEPcr7y1
v7rAq2l1R6qbrMFeBxyiaU4+XvOuP1THEIJjk8y5dqM6zzMgh2LydpLZ/g==
-----END PUBLIC KEY-----'

DEFAULT_BASE_URL="https://get.tumika.org"
BASE_URL="${TUMIKA_BASE_URL:-$DEFAULT_BASE_URL}"
BASE_URL="${BASE_URL%/}"
# Where the daemon listens unless it was told otherwise — the same constant the
# app is compiled with (DEFAULT_BASE_URL in source/desktop/src-tauri/src/health.rs).
DEFAULT_DAEMON_URL="http://127.0.0.1:8737"
# Keychain coordinates the daemon writes to (platform/tokencustody), read the
# way the app reads them (src-tauri/src/keychain.rs).
KEYCHAIN_SERVICE="tumika"
KEYCHAIN_ACCOUNT="api-token"
# go-keyring stores a value behind this marker, followed by standard base64.
KEYCHAIN_BASE64_PREFIX="go-keyring-base64:"
# `security` exits with errSecItemNotFound when there is no item to read.
KEYCHAIN_ITEM_NOT_FOUND=44

# A release tag is v<label>; accepting the tag costs one substitution.
LABEL="${TUMIKA_RELEASE:-}"
LABEL="${LABEL#v}"

# A seam is refused, not ignored quietly: an operator who set one against the
# real site believes an install was verified with their key, or read from their
# daemon, and it was not.
if [ "$BASE_URL" = "$DEFAULT_BASE_URL" ]; then
  for seam in TUMIKA_TRUSTED_KEY_FILE TUMIKA_DAEMON_URL TUMIKA_SECURITY_BIN TUMIKA_UNAME_S TUMIKA_UNAME_M; do
    eval "value=\${$seam:-}"
    if [ -n "$value" ]; then
      echo "error: $seam is a test seam and is only honoured with TUMIKA_BASE_URL pointing elsewhere" >&2
      exit 1
    fi
  done
fi
DAEMON_URL="${TUMIKA_DAEMON_URL:-$DEFAULT_DAEMON_URL}"
DAEMON_URL="${DAEMON_URL%/}"
SECURITY_BIN="${TUMIKA_SECURITY_BIN:-/usr/bin/security}"

# The site is https and nothing else. The fixture seam serves plain http, and
# whoever set it already chooses every byte, so the relaxation goes no further
# than a non-default host.
if [ "$BASE_URL" = "$DEFAULT_BASE_URL" ]; then
  SITE_PROTO="=https"
  ASSET_URL_SHAPE='^https://[A-Za-z0-9._~%:/?#@!$&()*+,;=-]+$'
else
  SITE_PROTO="=http,https"
  ASSET_URL_SHAPE='^https?://[A-Za-z0-9._~%:/?#@!$&()*+,;=-]+$'
fi

# Whatever the platform, the operating system decides first: a Linux user
# running this has the wrong script, and the message has to say which one is
# right before anything is fetched on their behalf.
OS=$(printf '%s' "${TUMIKA_UNAME_S:-$(uname -s)}" | tr '[:upper:]' '[:lower:]')
if [ "$OS" != "darwin" ]; then
  echo "error: the tumika desktop app is macOS-only, and this is $OS" >&2
  echo "the daemon runs here: curl -fsSL $BASE_URL/install-daemon.sh | sh" >&2
  exit 1
fi

ARCH="${TUMIKA_UNAME_M:-$(uname -m)}"
case "$ARCH" in
  x86_64 | amd64) ARCH=amd64 ;;
  arm64 | aarch64) ARCH=arm64 ;;
  *)
    echo "error: unsupported architecture: $ARCH" >&2
    exit 1
    ;;
esac
# The key an asset is published under, Go's own GOOS_GOARCH.
PLATFORM="darwin_${ARCH}"

command -v curl >/dev/null 2>&1 || {
  echo "error: curl is required to fetch a release" >&2
  exit 1
}
# Refuse rather than install unverified: without openssl the signature over the
# bill of materials cannot be checked, and the document is the only thing that
# says which bytes are tumika's.
command -v openssl >/dev/null 2>&1 || {
  echo "error: openssl is required to verify the release signature" >&2
  exit 1
}
command -v tar >/dev/null 2>&1 || {
  echo "error: tar is required to unpack the app" >&2
  exit 1
}

# ~/Applications, created if it does not exist. The staging directory lives
# inside it so that the final move is a rename on the same filesystem: a rename
# is atomic, while mv across filesystems degrades to a copy that leaves a
# half-written bundle behind if it fails.
APPS_DIR="$HOME/Applications"
mkdir -p "$APPS_DIR"
STAGE=$(mktemp -d "$APPS_DIR/.tumika-install.XXXXXX")
trap 'rm -rf "$STAGE"' EXIT
trap 'rm -rf "$STAGE"; exit 130' INT TERM HUP

KEY_FILE="$STAGE/release.pem"
if [ -n "${TUMIKA_TRUSTED_KEY_FILE:-}" ]; then
  if [ ! -r "$TUMIKA_TRUSTED_KEY_FILE" ]; then
    echo "error: TUMIKA_TRUSTED_KEY_FILE names $TUMIKA_TRUSTED_KEY_FILE, which cannot be read" >&2
    exit 1
  fi
  cp "$TUMIKA_TRUSTED_KEY_FILE" "$KEY_FILE"
else
  printf '%s\n' "$RELEASE_PUBLIC_KEY" >"$KEY_FILE"
fi

# The shape a release label may have, before it is put in a URL path. Source of
# truth: releaseLabelPattern in
# source/daemon/internal/platform/release/bom.go. No character that could escape
# a path segment matches, which is what makes the label the daemon reports — or
# the one in TUMIKA_RELEASE — safe to address a document with.
RELEASE_LABEL_SHAPE='^([0-9]{4}\.[0-9]{2}\.[0-9]{2}(-beta\.[0-9]{1,6})?|edge\.[0-9]{1,10})$'

if [ -n "$LABEL" ]; then
  if ! printf '%s\n' "$LABEL" | grep -Eq "$RELEASE_LABEL_SHAPE"; then
    echo "error: '$LABEL' is not a tumika release label (YYYY.MM.NN, YYYY.MM.NN-beta.N or edge.N)" >&2
    exit 1
  fi
  echo "installing release $LABEL, as TUMIKA_RELEASE asks"
else
  # Every route is behind the bearer token, /v1/version included, so the
  # release cannot be read without one.
  TOKEN=""
  SECURITY_STATUS=0
  TOKEN=$("$SECURITY_BIN" find-generic-password -s "$KEYCHAIN_SERVICE" -a "$KEYCHAIN_ACCOUNT" -w \
    2>"$STAGE/security.err") || SECURITY_STATUS=$?
  if [ "$SECURITY_STATUS" -eq "$KEYCHAIN_ITEM_NOT_FOUND" ]; then
    echo "error: the login Keychain holds no tumika API token (service $KEYCHAIN_SERVICE, account $KEYCHAIN_ACCOUNT)" >&2
    echo "install and start the daemon first: curl -fsSL $BASE_URL/install-daemon.sh | sh" >&2
    exit 1
  fi
  if [ "$SECURITY_STATUS" -ne 0 ]; then
    echo "error: reading the API token from the login Keychain failed: $(cat "$STAGE/security.err")" >&2
    exit 1
  fi

  # A stored value may be base64 behind go-keyring's marker, or the plain text
  # it wrote verbatim. Neither the value nor any part of it is printed here or
  # anywhere below: the error messages name the failure, never the token.
  case "$TOKEN" in
    "$KEYCHAIN_BASE64_PREFIX"*)
      TOKEN=$(printf '%s' "${TOKEN#"$KEYCHAIN_BASE64_PREFIX"}" | openssl base64 -d -A 2>/dev/null) || {
        echo "error: the API token in the login Keychain is not valid base64" >&2
        exit 1
      }
      ;;
  esac
  if [ -z "$TOKEN" ]; then
    echo "error: the API token in the login Keychain is empty" >&2
    exit 1
  fi
  # The token goes into a curl config on stdin, whose quoted values take
  # backslash escapes. A token carrying a quote or a backslash would change the
  # meaning of that line, so it is refused rather than escaped — a tumika token
  # is `tmk_` and base64url, and anything else is not one.
  case "$TOKEN" in
    *[!A-Za-z0-9._~+/=-]*)
      echo "error: the API token in the login Keychain is not in the shape tumika issues" >&2
      exit 1
      ;;
  esac

  echo "asking the daemon at $DAEMON_URL which release it runs..."
  # The token reaches curl on stdin, never in argv and never in a file: `ps`
  # shows the URL and nothing else, and nothing is left on disk to remove.
  VERSION_STATUS=0
  VERSION_JSON=$(printf 'header = "Authorization: Bearer %s"\n' "$TOKEN" |
    curl --fail --silent --show-error --location --proto "=http,https" \
      --connect-timeout 5 --max-time 15 --max-filesize 65536 \
      --config - "$DAEMON_URL/v1/version" 2>"$STAGE/daemon.err") || VERSION_STATUS=$?
  TOKEN=""
  if [ "$VERSION_STATUS" -ne 0 ]; then
    echo "error: $DAEMON_URL/v1/version could not be read: $(cat "$STAGE/daemon.err")" >&2
    echo "the app installs the release its daemon runs, so a daemon that does not answer is the first thing to fix" >&2
    echo "install it with: curl -fsSL $BASE_URL/install-daemon.sh | sh" >&2
    exit 1
  fi

  LABEL=$(printf '%s' "$VERSION_JSON" | grep -o '"release"[[:space:]]*:[[:space:]]*"[^"]*"' |
    head -1 | awk -F'"' '{ print $4 }')
  CHANNEL=$(printf '%s' "$VERSION_JSON" | grep -o '"channel"[[:space:]]*:[[:space:]]*"[^"]*"' |
    head -1 | awk -F'"' '{ print $4 }')
  if [ -z "$LABEL" ]; then
    echo "error: $DAEMON_URL/v1/version answered without a release; it is not a tumika daemon" >&2
    exit 1
  fi
  # A daemon built outside a release reports "dev", and every other shape is a
  # string the daemon's own build stamp supplied. Both are refused here, before
  # the label reaches a URL.
  if ! printf '%s\n' "$LABEL" | grep -Eq "$RELEASE_LABEL_SHAPE"; then
    echo "error: the daemon at $DAEMON_URL runs release '$LABEL', which is not a release label" >&2
    echo "pass TUMIKA_RELEASE=<label> to install a published release instead" >&2
    exit 1
  fi
  if [ -n "$CHANNEL" ]; then
    echo "the daemon runs release $LABEL on the $CHANNEL channel"
  else
    echo "the daemon runs release $LABEL"
  fi
fi

DOC_URL="$BASE_URL/releases/$LABEL.json"
echo "fetching release $LABEL from $BASE_URL..."
if ! curl --fail --silent --show-error --location --proto "$SITE_PROTO" \
  --connect-timeout 10 --max-time 60 --max-filesize 4194304 \
  -o "$STAGE/bom.json" "$DOC_URL"; then
  echo "error: no bill of materials at $DOC_URL; release $LABEL offers nothing" >&2
  exit 1
fi
# The signature is detached, so its absence is a document nobody vouched for.
# That is refused exactly as a bad signature is.
if ! curl --fail --silent --show-error --location --proto "$SITE_PROTO" \
  --connect-timeout 10 --max-time 60 --max-filesize 65536 \
  -o "$STAGE/bom.sig" "$DOC_URL.sig"; then
  echo "error: the bill of materials at $DOC_URL is unsigned: $DOC_URL.sig is not published" >&2
  exit 1
fi

# The signature file is base64 of an ASN.1 DER ECDSA signature on one line, and
# -A is what reads a line of any length. A build without -A decodes 64-column
# input only, and stops at the first longer line WITHOUT failing — a short DER
# that then reports as a bad signature rather than an unreadable one. The
# fallback folds the input first so both paths decode the same bytes.
if ! openssl base64 -d -A -in "$STAGE/bom.sig" -out "$STAGE/bom.der" 2>/dev/null; then
  fold -w 64 "$STAGE/bom.sig" >"$STAGE/bom.folded"
  if ! openssl base64 -d -in "$STAGE/bom.folded" -out "$STAGE/bom.der" 2>/dev/null; then
    echo "error: the signature at $DOC_URL.sig is not base64" >&2
    exit 1
  fi
fi
# Some openssl builds decode an unreadable signature to nothing and report
# success, so the decoded file is checked rather than the exit status alone.
if [ ! -s "$STAGE/bom.der" ]; then
  echo "error: the signature at $DOC_URL.sig is empty or is not base64" >&2
  exit 1
fi

# ECDSA P-256 over the SHA-256 of the document's exact bytes. Everything below
# this line reads a document that verified.
if ! openssl dgst -sha256 -verify "$KEY_FILE" -signature "$STAGE/bom.der" "$STAGE/bom.json" >/dev/null 2>&1; then
  echo "error: the bill of materials at $DOC_URL does not verify against the tumika release key" >&2
  exit 1
fi
echo "verified the signature on $DOC_URL"

# A verified document still has to be the one that was asked for: a signed
# document served under another label's name is a valid signature over the wrong
# answer, and pairing an app with a release the daemon does not run is the one
# thing this installer exists to prevent.
DOC_RELEASE=$(awk -F'"' '/^  "release": "/ { print $4; exit }' "$STAGE/bom.json")
if [ "$DOC_RELEASE" != "$LABEL" ]; then
  echo "error: $DOC_URL describes release $DOC_RELEASE, not $LABEL" >&2
  exit 1
fi

# The document is fixed-indent JSON written by tumika-bom: two spaces per level,
# one key per line. Indentation is what identifies a level here, so a "version"
# belonging to a component is never confused with one nested deeper, and a
# component named after a platform key cannot be read as an asset.
awk '
  # The name a `"<name>": {` line opens.
  function key(line) {
    sub(/^[[:space:]]*"/, "", line)
    sub(/".*$/, "", line)
    return line
  }
  # The string a `"<name>": "<value>"[,]` line carries.
  function value(line) {
    sub(/^[^:]*:[[:space:]]*"/, "", line)
    sub(/",?[[:space:]]*$/, "", line)
    return line
  }
  /^  "components": \{$/ { components = 1; next }
  components && /^  \}/ { components = 0; next }
  components && /^    "[^"]+": \{$/ { component = key($0); assets = 0; next }
  component != "desktop" { next }
  /^      "version": "/ { print "version " value($0) }
  /^      "assets": \{$/ { assets = 1; next }
  assets && /^      \}/ { assets = 0; next }
  assets && /^        "[^"]+": \{$/ { platform = key($0); next }
  assets && /^          "url": "/ { print platform " url " value($0) }
  assets && /^          "sha256": "/ { print platform " sha256 " value($0) }
' "$STAGE/bom.json" >"$STAGE/desktop.txt"

COMPONENT_VERSION=$(awk '$1 == "version" { print $2; exit }' "$STAGE/desktop.txt")
if [ -z "$COMPONENT_VERSION" ]; then
  echo "error: release $DOC_RELEASE ships no desktop component" >&2
  exit 1
fi
ASSET_URL=$(awk -v p="$PLATFORM" '$1 == p && $2 == "url" { print $3; exit }' "$STAGE/desktop.txt")
WANT=$(awk -v p="$PLATFORM" '$1 == p && $2 == "sha256" { print $3; exit }' "$STAGE/desktop.txt")
if [ -z "$ASSET_URL" ] || [ -z "$WANT" ]; then
  echo "error: release $DOC_RELEASE publishes no desktop asset for $PLATFORM" >&2
  exit 1
fi

# The asset's `signature` field is the minisign document the Tauri updater
# verifies an update with. It is deliberately not read here: this install is
# covered by the release signature over the document and by the SHA-256 the
# document carries, and the updater's key is the app's business, not the
# installer's.

# Shape checks on values that are about to become a URL and a comparison. They
# are not the verification — that already happened — but a malformed document
# should fail with its reason rather than as a 404 on a nonsense URL.
if ! printf '%s\n' "$ASSET_URL" | grep -Eq "$ASSET_URL_SHAPE"; then
  echo "error: release $DOC_RELEASE gives the $PLATFORM asset url as '$ASSET_URL', which is not an https url" >&2
  exit 1
fi
if ! printf '%s\n' "$WANT" | grep -Eq '^[0-9a-f]{64}$'; then
  echo "error: release $DOC_RELEASE gives the $PLATFORM checksum as '$WANT', which is not a sha256 digest" >&2
  exit 1
fi

echo "downloading Tumika ${COMPONENT_VERSION} from release ${DOC_RELEASE} (darwin/${ARCH})..."
# curl sets no com.apple.quarantine attribute, which is what lets an ad-hoc
# signed bundle open without a Gatekeeper prompt. Nothing here may be replaced
# by a downloader that does set one.
if ! curl --fail --silent --show-error --location --proto "$SITE_PROTO" \
  --connect-timeout 10 --max-time 600 --max-filesize 268435456 \
  -o "$STAGE/app.tar.gz" "$ASSET_URL"; then
  echo "error: release $DOC_RELEASE names $ASSET_URL, which could not be downloaded" >&2
  exit 1
fi

if command -v shasum >/dev/null 2>&1; then
  GOT=$(shasum -a 256 "$STAGE/app.tar.gz" | awk '{ print $1 }')
elif command -v sha256sum >/dev/null 2>&1; then
  GOT=$(sha256sum "$STAGE/app.tar.gz" | awk '{ print $1 }')
else
  # Refuse rather than install unverified. The digest is what ties the signed
  # document to the bytes on disk; without it the signature covers a URL and
  # nothing more.
  echo "error: neither shasum nor sha256sum is available; cannot verify the download" >&2
  exit 1
fi
if [ "$GOT" != "$WANT" ]; then
  echo "error: checksum mismatch for the $PLATFORM desktop asset of release $DOC_RELEASE: got $GOT, want $WANT" >&2
  exit 1
fi

# The archive is read before it is written out. tar extracts an absolute path or
# a `..` component wherever it points, so an archive carrying one writes outside
# the staging directory entirely — and the top-level name is checked too,
# because what lands in ~/Applications is a bundle by that exact name.
if ! tar -tzf "$STAGE/app.tar.gz" >"$STAGE/entries.txt" 2>/dev/null; then
  echo "error: the $PLATFORM desktop asset of release $DOC_RELEASE is not a gzip tar archive" >&2
  exit 1
fi
if [ ! -s "$STAGE/entries.txt" ]; then
  echo "error: the $PLATFORM desktop asset of release $DOC_RELEASE is an empty archive" >&2
  exit 1
fi
while IFS= read -r ENTRY; do
  case "$ENTRY" in
    "" | ./) continue ;;
    # Any entry containing `..` is refused, not only a `..` path component: the
    # bundles tumika ships carry no such name, so the broader rule costs
    # nothing and leaves no spelling to find.
    /* | *..*)
      echo "error: the $PLATFORM desktop asset of release $DOC_RELEASE carries the entry '$ENTRY', which escapes its directory" >&2
      exit 1
      ;;
  esac
  case "${ENTRY#./}" in
    Tumika.app | Tumika.app/*) ;;
    *)
      echo "error: the $PLATFORM desktop asset of release $DOC_RELEASE carries '$ENTRY'; it holds Tumika.app and nothing else" >&2
      exit 1
      ;;
  esac
done <"$STAGE/entries.txt"

mkdir "$STAGE/unpack"
if ! tar -xzf "$STAGE/app.tar.gz" -C "$STAGE/unpack"; then
  echo "error: the $PLATFORM desktop asset of release $DOC_RELEASE could not be unpacked" >&2
  exit 1
fi
if [ ! -d "$STAGE/unpack/Tumika.app" ]; then
  echo "error: the $PLATFORM desktop asset of release $DOC_RELEASE unpacked without a Tumika.app bundle" >&2
  exit 1
fi

# The replacement is two renames on one filesystem, in the order that leaves
# something openable at every point: the bundle in place is moved aside first,
# and put back if the new one cannot take its place.
DEST="$APPS_DIR/Tumika.app"
REPLACED="$STAGE/Tumika.app.replaced"
if [ -e "$DEST" ]; then
  if ! mv "$DEST" "$REPLACED"; then
    echo "error: $DEST is in the way and could not be moved aside" >&2
    exit 1
  fi
fi
if ! mv "$STAGE/unpack/Tumika.app" "$DEST"; then
  echo "error: Tumika ${COMPONENT_VERSION} could not be moved into $APPS_DIR" >&2
  if [ -e "$REPLACED" ] && mv "$REPLACED" "$DEST"; then
    echo "the app that was there is back in place" >&2
  fi
  exit 1
fi
rm -rf "$REPLACED"

echo "installed Tumika ${COMPONENT_VERSION} from release ${DOC_RELEASE} to $DEST"
echo
echo "next: open $DEST"
# Replacing the bundle does not reach into a process that is already running it:
# macOS keeps the open files alive, so the old component version keeps running
# until someone restarts it.
echo "note: a Tumika already running keeps its own component version until you quit it and open it again"

}

main "$@"
