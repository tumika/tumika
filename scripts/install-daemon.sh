#!/bin/sh
# install-daemon.sh — install the tumika daemon from a signed bill of materials.
#
# Usage:
#   curl -fsSL https://get.tumika.org/install-daemon.sh | sh
#
# It fetches a channel's bill of materials and the detached signature beside it,
# verifies that signature against the release public key embedded below, and
# only then reads the asset URL and its SHA-256 out of the document. Nothing the
# document says reaches a URL, a path or the disk before it verifies, so a
# substituted or tampered document installs nothing at all.
#
# Environment:
#   TUMIKA_CHANNEL      stable, beta or edge (default: stable). Channels are
#                       cumulative: beta receives what stable publishes, edge
#                       receives both.
#   TUMIKA_VERSION      a release LABEL to pin — 2026.09.01, 2026.09.01-beta.1
#                       or edge.417 (a leading v is accepted and dropped). It
#                       replaces the channel lookup: that release is installed
#                       whichever channel it was cut into, so TUMIKA_CHANNEL is
#                       not consulted.
#   TUMIKA_INSTALL_DIR  install directory (default: $HOME/.local/bin)
#   TUMIKA_BASE_URL     host the documents and the assets are read from
#                       (default: https://get.tumika.org). A TEST SEAM: it
#                       points the script at a fixture server.
#   TUMIKA_TRUSTED_KEY_FILE
#                       a PEM public key to verify the document with in place of
#                       the embedded one. A TEST SEAM, honoured ONLY when
#                       TUMIKA_BASE_URL names a host other than the default. An
#                       install reading the real site therefore trusts the
#                       embedded key and nothing else, and whoever can point
#                       TUMIKA_BASE_URL elsewhere already chooses every byte the
#                       script sees.
#
# This installs the BINARY. Running it as a supervised service is a second step,
# and it differs by platform:
#
#   Linux:  sudo <install-dir>/tumika install   # writes a systemd unit, creates
#                                               # the service account
#   macOS:  tumika install                      # a LaunchAgent, which must run
#                                               # as YOU — it needs your login
#                                               # session to reach the Keychain
#
# The script prints whichever applies when it finishes.
set -eu

# Everything lives in main(), called on the LAST line.
#
# `curl … | sh` executes statements as they arrive, so a connection dropped
# mid-transfer runs whatever prefix landed. With a wrapper, a truncated download
# parses to a function definition that is never called, and does nothing at all.
main() {

# The release public key. It is the first entry of releaseKeyPEMs in
# source/daemon/internal/platform/release/keys.go, which is the list the daemon
# verifies its own updates against; TestInstallerKeyMatchesReleaseKey fails when
# the two drift. Rotating the key means editing both.
RELEASE_PUBLIC_KEY='-----BEGIN PUBLIC KEY-----
MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEGgmfTHgWRmKZRGo7FrLeAEPcr7y1
v7rAq2l1R6qbrMFeBxyiaU4+XvOuP1THEIJjk8y5dqM6zzMgh2LydpLZ/g==
-----END PUBLIC KEY-----'

DEFAULT_BASE_URL="https://get.tumika.org"
BASE_URL="${TUMIKA_BASE_URL:-$DEFAULT_BASE_URL}"
BASE_URL="${BASE_URL%/}"
INSTALL_DIR="${TUMIKA_INSTALL_DIR:-$HOME/.local/bin}"
CHANNEL="${TUMIKA_CHANNEL:-stable}"
# A release tag is v<label>; accepting the tag costs one substitution.
LABEL="${TUMIKA_VERSION:-}"
LABEL="${LABEL#v}"

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

OS=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$OS" in
  linux | darwin) ;;
  *)
    echo "error: tumika supports linux and darwin, not $OS" >&2
    exit 1
    ;;
esac

ARCH=$(uname -m)
case "$ARCH" in
  x86_64 | amd64) ARCH=amd64 ;;
  aarch64 | arm64) ARCH=arm64 ;;
  *)
    echo "error: unsupported architecture: $ARCH" >&2
    exit 1
    ;;
esac
# The key an asset is published under, Go's own GOOS_GOARCH.
PLATFORM="${OS}_${ARCH}"

# A pinned label addresses a release document; otherwise the channel's head
# does. Both are validated BEFORE they reach a URL — the same shapes
# release.ValidateReleaseLabel and release.ValidateChannel accept.
if [ -n "$LABEL" ]; then
  if ! printf '%s\n' "$LABEL" | grep -Eq \
    '^([0-9]{4}\.[0-9]{2}\.[0-9]{2}(-beta\.[0-9]{1,6})?|edge\.[0-9]{1,10})$'; then
    echo "error: '$LABEL' is not a tumika release label (YYYY.MM.NN, YYYY.MM.NN-beta.N or edge.N)" >&2
    exit 1
  fi
  DOC_URL="$BASE_URL/releases/$LABEL.json"
  WANTED="release $LABEL"
else
  case "$CHANNEL" in
    stable | beta | edge) ;;
    *)
      echo "error: '$CHANNEL' is not a tumika channel (stable, beta or edge)" >&2
      exit 1
      ;;
  esac
  DOC_URL="$BASE_URL/channels/$CHANNEL.json"
  WANTED="the $CHANNEL channel"
fi

# Staged INSIDE the install directory so the final move is a rename on the same
# filesystem. A rename is atomic and never truncates a running binary; mv across
# filesystems degrades to copy-then-truncate, which is exactly how you kill a
# daemon mid-write while it is executing the file being replaced.
mkdir -p "$INSTALL_DIR"
STAGE=$(mktemp -d "$INSTALL_DIR/.tumika-install.XXXXXX")
trap 'rm -rf "$STAGE"' EXIT

# The seam is refused, not ignored quietly: an operator who set it against the
# real host believes an install was verified with their key, and it was not.
KEY_FILE="$STAGE/release.pem"
if [ -n "${TUMIKA_TRUSTED_KEY_FILE:-}" ]; then
  if [ "$BASE_URL" = "$DEFAULT_BASE_URL" ]; then
    echo "error: TUMIKA_TRUSTED_KEY_FILE is a test seam and is only honoured with TUMIKA_BASE_URL pointing elsewhere" >&2
    exit 1
  fi
  if [ ! -r "$TUMIKA_TRUSTED_KEY_FILE" ]; then
    echo "error: TUMIKA_TRUSTED_KEY_FILE names $TUMIKA_TRUSTED_KEY_FILE, which cannot be read" >&2
    exit 1
  fi
  cp "$TUMIKA_TRUSTED_KEY_FILE" "$KEY_FILE"
else
  printf '%s\n' "$RELEASE_PUBLIC_KEY" >"$KEY_FILE"
fi

echo "fetching $WANTED from $BASE_URL..."
if ! curl -fsSL -o "$STAGE/bom.json" "$DOC_URL"; then
  echo "error: no bill of materials at $DOC_URL; $WANTED offers nothing" >&2
  exit 1
fi
# The signature is detached, so its absence is a document nobody vouched for.
# That is refused exactly as a bad signature is.
if ! curl -fsSL -o "$STAGE/bom.sig" "$DOC_URL.sig"; then
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
# stable head served from the edge path, or a signed release document under
# another label's name, is a valid signature over the wrong answer.
DOC_RELEASE=$(awk -F'"' '/^  "release": "/ { print $4; exit }' "$STAGE/bom.json")
DOC_CHANNEL=$(awk -F'"' '/^  "channel": "/ { print $4; exit }' "$STAGE/bom.json")
if ! printf '%s\n' "$DOC_RELEASE" | grep -Eq \
  '^([0-9]{4}\.[0-9]{2}\.[0-9]{2}(-beta\.[0-9]{1,6})?|edge\.[0-9]{1,10})$'; then
  echo "error: $DOC_URL names release '$DOC_RELEASE', which is not a release label" >&2
  exit 1
fi
if [ -n "$LABEL" ] && [ "$DOC_RELEASE" != "$LABEL" ]; then
  echo "error: $DOC_URL describes release $DOC_RELEASE, not $LABEL" >&2
  exit 1
fi
if [ -z "$LABEL" ] && [ "$DOC_CHANNEL" != "$CHANNEL" ]; then
  echo "error: $DOC_URL is the head of channel '$DOC_CHANNEL', not $CHANNEL" >&2
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
  component != "daemon" { next }
  /^      "version": "/ { print "version " value($0) }
  /^      "assets": \{$/ { assets = 1; next }
  assets && /^      \}/ { assets = 0; next }
  assets && /^        "[^"]+": \{$/ { platform = key($0); next }
  assets && /^          "url": "/ { print platform " url " value($0) }
  assets && /^          "sha256": "/ { print platform " sha256 " value($0) }
' "$STAGE/bom.json" >"$STAGE/daemon.txt"

COMPONENT_VERSION=$(awk '$1 == "version" { print $2; exit }' "$STAGE/daemon.txt")
if [ -z "$COMPONENT_VERSION" ]; then
  echo "error: release $DOC_RELEASE ships no daemon component" >&2
  exit 1
fi
ASSET_URL=$(awk -v p="$PLATFORM" '$1 == p && $2 == "url" { print $3; exit }' "$STAGE/daemon.txt")
WANT=$(awk -v p="$PLATFORM" '$1 == p && $2 == "sha256" { print $3; exit }' "$STAGE/daemon.txt")
if [ -z "$ASSET_URL" ] || [ -z "$WANT" ]; then
  echo "error: release $DOC_RELEASE publishes no daemon asset for $PLATFORM" >&2
  exit 1
fi

# Shape checks on values that are about to become a URL and a comparison. They
# are not the verification — that already happened — but a malformed document
# should fail with its reason rather than as a 404 on a nonsense URL.
if ! printf '%s\n' "$ASSET_URL" | grep -Eq '^https?://[A-Za-z0-9._~%:/?#@!$&()*+,;=-]+$'; then
  echo "error: release $DOC_RELEASE gives the $PLATFORM asset url as '$ASSET_URL', which is not an http(s) url" >&2
  exit 1
fi
if ! printf '%s\n' "$WANT" | grep -Eq '^[0-9a-f]{64}$'; then
  echo "error: release $DOC_RELEASE gives the $PLATFORM checksum as '$WANT', which is not a sha256 digest" >&2
  exit 1
fi

echo "downloading tumika ${COMPONENT_VERSION} from release ${DOC_RELEASE} (${OS}/${ARCH})..."
if ! curl -fsSL -o "$STAGE/tumika" "$ASSET_URL"; then
  echo "error: release $DOC_RELEASE names $ASSET_URL, which could not be downloaded" >&2
  exit 1
fi

if command -v sha256sum >/dev/null 2>&1; then
  GOT=$(sha256sum "$STAGE/tumika" | awk '{ print $1 }')
elif command -v shasum >/dev/null 2>&1; then
  GOT=$(shasum -a 256 "$STAGE/tumika" | awk '{ print $1 }')
else
  # Refuse rather than install unverified. The digest is what ties the signed
  # document to the bytes on disk; without it the signature covers a URL and
  # nothing more.
  echo "error: neither sha256sum nor shasum is available; cannot verify the download" >&2
  exit 1
fi

if [ "$GOT" != "$WANT" ]; then
  echo "error: checksum mismatch for the $PLATFORM asset of release $DOC_RELEASE: got $GOT, want $WANT" >&2
  exit 1
fi

chmod 0755 "$STAGE/tumika"
mv "$STAGE/tumika" "$INSTALL_DIR/tumika"
echo "installed tumika ${COMPONENT_VERSION} from release ${DOC_RELEASE} to $INSTALL_DIR/tumika"

case ":$PATH:" in
  *":$INSTALL_DIR:"*) ;;
  *) echo "note: $INSTALL_DIR is not on your PATH" ;;
esac

# The next step differs by platform, and the obvious hint is wrong on both.
#
# On Linux the install needs root — but sudo's secure_path replaces PATH on
# Debian, Ubuntu and RHEL, so a bare `sudo tumika install` is
# "sudo: tumika: command not found" even when the PATH note above stays quiet.
# The full path is what actually works.
#
# On macOS it installs a LaunchAgent, which must run as the OPERATOR: sudo there
# would install the agent for root, and the daemon would never see the login
# Keychain that holds its credentials.
echo
if [ "$OS" = "darwin" ]; then
  echo "next: tumika install   # run it as a LaunchAgent (no sudo: it needs your login session)"
else
  echo "next: sudo $INSTALL_DIR/tumika install   # run it as a systemd service"
fi

}

main "$@"
