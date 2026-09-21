#!/usr/bin/env bash
# Assembles the tree get.tumika.org serves, and refuses to produce a broken one.
#
# Four inputs land in one directory: the signed BOM tree tumika-bom wrote, the
# two installers, and the static files under scripts/site. Each installer is
# passed by path and always lands under its documented name — install-daemon.sh
# and install-app.sh — so the URL does not depend on where the file was built.
# One Pages site is one host, so both live here.
#
# Every check below describes a site that deploys cleanly and then fails in a
# way nobody watching the workflow can see: a document without its signature is
# refused by every daemon, a missing CNAME hands the apex back to
# <owner>.github.io and breaks every hard-coded URL at once, and an empty
# channels/ answers 404 to the one question the host exists to answer. Pages
# replaces the whole site on each deploy, so each of those takes the channel
# down until the next successful run.
#
# Usage: scripts/assemble-site.sh <bom-dir> <daemon-installer> <app-installer> <static-dir> <out-dir>
set -euo pipefail

fail() { echo "FAIL: $*" >&2; exit 1; }

[[ $# -eq 5 ]] || fail "usage: $0 <bom-dir> <daemon-installer> <app-installer> <static-dir> <out-dir>"

BOM_DIR="$1"
DAEMON_INSTALLER="$2"
APP_INSTALLER="$3"
STATIC_DIR="$4"
OUT_DIR="$5"

# An installer is built by a different step and is the input most likely to be
# absent, so it is named plainly rather than reported as a missing file.
[[ -d "$BOM_DIR" ]] || fail "$BOM_DIR is not a directory; tumika-bom writes the BOM tree"
[[ -f "$DAEMON_INSTALLER" ]] || fail "$DAEMON_INSTALLER is not a file; the site serves it as /install-daemon.sh"
[[ -f "$APP_INSTALLER" ]] || fail "$APP_INSTALLER is not a file; the site serves it as /install-app.sh"
[[ -d "$STATIC_DIR" ]] || fail "$STATIC_DIR is not a directory; it holds the site's static files"

# A fresh directory is what makes the checks below cover the whole published
# tree: assembling into an existing one would carry leftovers from an earlier
# run past every one of them.
[[ ! -e "$OUT_DIR" ]] || fail "$OUT_DIR already exists; assemble into a fresh directory"
mkdir -p "$OUT_DIR"

cp -R "$BOM_DIR/." "$OUT_DIR/"
cp "$DAEMON_INSTALLER" "$OUT_DIR/install-daemon.sh"
cp "$APP_INSTALLER" "$OUT_DIR/install-app.sh"
chmod 0755 "$OUT_DIR/install-daemon.sh" "$OUT_DIR/install-app.sh"
# Last, so a static file wins over anything of the same name in the BOM tree.
cp -R "$STATIC_DIR/." "$OUT_DIR/"

[[ -f "$OUT_DIR/index.html" ]] || fail "$STATIC_DIR carries no index.html"

# One line, one host. An empty or multi-line CNAME is accepted by Pages as "no
# custom domain", which silently moves the site off get.tumika.org.
[[ -f "$OUT_DIR/CNAME" ]] || fail "$STATIC_DIR carries no CNAME; the site needs the custom domain"
hosts=$(grep -c '[^[:space:]]' "$OUT_DIR/CNAME" || true)
[[ "$hosts" -eq 1 ]] || fail "CNAME names $hosts hosts; it names exactly one"

# The site answers channel queries. Deploying a tree without one replaces a
# working host with 404s, which every daemon reads as "no release available".
shopt -s nullglob
channels=("$OUT_DIR"/channels/*.json)
shopt -u nullglob
[[ "${#channels[@]}" -gt 0 ]] || fail "$BOM_DIR has no channels/*.json; there is nothing to serve"

# Signatures are detached, so a document and its .sig are two writes and either
# can be the one that did not happen.
signed=()
for directory in "$OUT_DIR/channels" "$OUT_DIR/releases"; do
  if [[ -d "$directory" ]]; then signed+=("$directory"); fi
done
while IFS= read -r document; do
  [[ -f "$document.sig" ]] || fail "${document#"$OUT_DIR/"} has no matching .sig"
done < <(find "${signed[@]}" -type f -name '*.json')

echo "assembled $OUT_DIR: ${#channels[@]} channel head(s), install-daemon.sh, install-app.sh, $(basename "$STATIC_DIR") static files"
