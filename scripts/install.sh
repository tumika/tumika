#!/bin/sh
# install.sh — download and install tumika from GitHub releases.
#
# Usage:
#   curl -fsSL https://github.com/tumika/tumika/releases/latest/download/install.sh | sh
#
# Environment:
#   TUMIKA_VERSION      the RELEASE TAG to install, with or without the leading
#                       v — e.g. v2026.09.01, 2026.09.01, v2026.09.01-beta.1
#                       (default: latest). A release tag is a calendar label and
#                       is NOT a component version: the binary's own semver is
#                       read from that release's release.yaml asset.
#   TUMIKA_INSTALL_DIR  install directory (default: $HOME/.local/bin)
#   TUMIKA_RELEASES_URL base URL the release assets are fetched from (default:
#                       https://github.com/tumika/tumika). A mirror must serve
#                       releases/download/<tag>/<asset>; resolving "latest" also
#                       needs releases/latest to redirect to the tag page, so
#                       against a mirror that does not, name a tag explicitly.
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

REPO_URL="${TUMIKA_RELEASES_URL:-https://github.com/tumika/tumika}"
INSTALL_DIR="${TUMIKA_INSTALL_DIR:-$HOME/.local/bin}"
TAG="${TUMIKA_VERSION:-latest}"

if [ "$TAG" = "latest" ]; then
  # The releases/latest URL redirects to .../releases/tag/<tag>.
  LOCATION=$(curl -fsSI -o /dev/null -w '%{redirect_url}' "$REPO_URL/releases/latest")
  TAG="${LOCATION##*/}"
fi
# A release tag is v<label>; accepting it without the v costs one substitution.
TAG="v${TAG#v}"
if [ "$TAG" = "v" ]; then
  echo "error: could not resolve a tumika release tag" >&2
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

# Staged INSIDE the install directory so the final move is a rename on the same
# filesystem. A rename is atomic and never truncates a running binary; mv across
# filesystems degrades to copy-then-truncate, which is exactly how you kill a
# daemon mid-write while it is executing the file being replaced.
mkdir -p "$INSTALL_DIR"
STAGE=$(mktemp -d "$INSTALL_DIR/.tumika-install.XXXXXX")
trap 'rm -rf "$STAGE"' EXIT

# The tag names the release; the asset is named after the DAEMON COMPONENT
# VERSION, which only the release's own release.yaml knows. A calendar label and
# a component semver advance independently, so neither can be derived from the
# other — the asset name has to be read, not guessed.
if ! curl -fsSL -o "$STAGE/release.yaml" "$REPO_URL/releases/download/${TAG}/release.yaml"; then
  echo "error: release $TAG has no release.yaml asset, so the asset name cannot be resolved" >&2
  exit 1
fi

# Entries of the top-level `components:` block are indented `name: version`
# lines and the block ends at the next line starting in column 1. \042 and \047
# are the double and single quote; a version contains neither.
COMPONENT_VERSION=$(awk '
  /^components:/ { inblock = 1; next }
  /^[^[:space:]#]/ { inblock = 0 }
  inblock && /^[[:space:]]+[^[:space:]#]/ {
    line = $0
    sub(/^[[:space:]]+/, "", line)
    sub(/[[:space:]]*#.*$/, "", line)
    n = index(line, ":")
    if (n == 0) next
    if (substr(line, 1, n - 1) != "daemon") next
    value = substr(line, n + 1)
    sub(/^[[:space:]]+/, "", value)
    sub(/[[:space:]]+$/, "", value)
    gsub(/["\047]/, "", value)
    print value
  }
' "$STAGE/release.yaml")

if [ -z "$COMPONENT_VERSION" ]; then
  echo "error: release.yaml for $TAG names no daemon version under 'components:'" >&2
  exit 1
fi

# A shape check, not the release gate: it is here so a malformed value fails
# with the reason rather than as a 404 on a nonsense asset name, and so nothing
# read out of a downloaded file can reach a URL or a path.
if ! printf '%s\n' "$COMPONENT_VERSION" | grep -Eq \
  '^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$'; then
  echo "error: release.yaml for $TAG gives the daemon version as '$COMPONENT_VERSION', which is not semver" >&2
  exit 1
fi

# The RAW archive: a single uncompressed binary, the same asset the self-updater
# fetches. The name template is fixed by .goreleaser.yml.
ASSET="tumika_${COMPONENT_VERSION}_${OS}_${ARCH}"

echo "downloading tumika ${COMPONENT_VERSION} from release ${TAG} (${OS}/${ARCH})..."
curl -fsSL -o "$STAGE/tumika" "$REPO_URL/releases/download/${TAG}/${ASSET}"
curl -fsSL -o "$STAGE/checksums.txt" "$REPO_URL/releases/download/${TAG}/checksums.txt"

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
echo "installed tumika ${COMPONENT_VERSION} from release ${TAG} to $INSTALL_DIR/tumika"

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
