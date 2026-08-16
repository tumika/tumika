#!/bin/sh
# install.sh — download and install tumika from GitHub releases.
#
# Usage:
#   curl -fsSL https://github.com/tumika/tumika/releases/latest/download/install.sh | sh
#
# Environment:
#   TUMIKA_VERSION      version to install, e.g. 0.1.0 or v0.1.0 (default: latest)
#   TUMIKA_INSTALL_DIR  install directory (default: $HOME/.local/bin)
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
# mid-transfer runs whatever prefix landed. The ordering happens to be safe today
# — every mutating statement is after the checksum comparison — but that is
# incidental, and one future edit that moves a mutation earlier loses it
# silently. With a wrapper, a truncated download parses to a function definition
# that is never called, and does nothing at all.
main() {

REPO_URL="https://github.com/tumika/tumika"
INSTALL_DIR="${TUMIKA_INSTALL_DIR:-$HOME/.local/bin}"
VERSION="${TUMIKA_VERSION:-latest}"

if [ "$VERSION" = "latest" ]; then
  # The releases/latest URL redirects to .../releases/tag/v<version>.
  LOCATION=$(curl -fsSI -o /dev/null -w '%{redirect_url}' "$REPO_URL/releases/latest")
  VERSION="${LOCATION##*/}"
fi
VERSION="${VERSION#v}"
if [ -z "$VERSION" ]; then
  echo "error: could not resolve a tumika version" >&2
  exit 1
fi

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

# The RAW archive: a single uncompressed binary, the same asset the self-updater
# fetches. The name template is fixed by .goreleaser.yml.
ASSET="tumika_${VERSION}_${OS}_${ARCH}"

# Staged INSIDE the install directory so the final move is a rename on the same
# filesystem. A rename is atomic and never truncates a running binary; mv across
# filesystems degrades to copy-then-truncate, which is exactly how you kill a
# daemon mid-write while it is executing the file being replaced.
mkdir -p "$INSTALL_DIR"
STAGE=$(mktemp -d "$INSTALL_DIR/.tumika-install.XXXXXX")
trap 'rm -rf "$STAGE"' EXIT

echo "downloading tumika v${VERSION} (${OS}/${ARCH})..."
curl -fsSL -o "$STAGE/tumika" "$REPO_URL/releases/download/v${VERSION}/${ASSET}"
curl -fsSL -o "$STAGE/checksums.txt" "$REPO_URL/releases/download/v${VERSION}/checksums.txt"

WANT=$(awk -v asset="$ASSET" '$2 == asset { print $1 }' "$STAGE/checksums.txt")
if [ -z "$WANT" ]; then
  echo "error: no checksum for $ASSET in checksums.txt" >&2
  exit 1
fi

if command -v sha256sum >/dev/null 2>&1; then
  GOT=$(sha256sum "$STAGE/tumika" | awk '{ print $1 }')
elif command -v shasum >/dev/null 2>&1; then
  GOT=$(shasum -a 256 "$STAGE/tumika" | awk '{ print $1 }')
else
  # Refuse rather than install unverified. The checksum is the only thing
  # standing between a truncated or substituted download and a binary that is
  # about to be given a subscription credential.
  echo "error: neither sha256sum nor shasum is available; cannot verify the download" >&2
  exit 1
fi

if [ "$GOT" != "$WANT" ]; then
  echo "error: checksum mismatch for $ASSET: got $GOT, want $WANT" >&2
  exit 1
fi

chmod 0755 "$STAGE/tumika"
mv "$STAGE/tumika" "$INSTALL_DIR/tumika"
echo "installed tumika v${VERSION} to $INSTALL_DIR/tumika"

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
