#!/usr/bin/env bash
# Checks that a desktop build was staged under exactly the asset names the bill
# of materials reads it by, and prints the checksums.txt lines for its archives.
#
# internal/bomgen resolves the desktop component by the name rule
# `tumika-desktop_<component version>_<goos>_<goarch>.app.tar.gz`, and refuses to
# publish an archive whose `<name>.sig` is missing — a signed component's entry
# would otherwise hand an updater bytes it has nothing to verify. Tauri names its
# own output `Tumika.app.tar.gz`, so the rename between the bundler and the
# release is the whole of that contract, and nothing else asserts it: a rename
# that drifts publishes a release the desktop app can never resolve, and the
# generator reports it as a component this release did not build rather than as
# an error.
#
# Per platform:
#
#   - the archive is a regular file, non-empty, and a gzip tar
#   - it contains `Tumika.app/Contents/Info.plist`, whose
#     CFBundleShortVersionString is the component version. The bundler writes
#     the version as given, prerelease included, and a bundler that strips the
#     segment leaves the core, so a beta or edge component version matches
#     either spelling and nothing else.
#   - the `.sig` beside it holds a minisign signature. Tauri writes the base64 of
#     the minisign document, and bomgen copies that text verbatim into the BOM's
#     `signature` field, so an empty or truncated file is a signature the updater
#     refuses at install time rather than here. Plain minisign text is accepted
#     too: what matters is that the file is a signature and not a stub.
#   - the directory holds those two files per platform and nothing else. It
#     becomes the release's asset list, so a stray file is an asset nobody
#     audited, and a symlink is whatever `gh release upload` follows it to.
#
# The BOM generator reads each asset's SHA-256 out of the release's
# checksums.txt, which goreleaser writes for the daemon's assets alone. With
# --checksums the lines to append are printed on STDOUT in goreleaser's format
# (`<sha256>  <name>`) and every other line this script writes goes to stderr, so
# the output can be redirected straight into a file. The `.sig` files get no
# line: the BOM carries their text, not a URL to fetch and check.
#
# Usage: scripts/verify-desktop-assets.sh [--platform <goos_goarch>]... [--checksums] <dir> <component-version>
#   --platform   a platform to require, repeatable; the default is both of
#                darwin_arm64 and darwin_amd64. One build leg stages and verifies
#                its own platform; the job that attaches them verifies the pair.
#   --checksums  print `<sha256>  <name>` for each archive on stdout
set -euo pipefail

fail() { echo "FAIL: $*" >&2; exit 1; }
ok()   { echo "  ok: $*" >&2; }

usage="usage: $0 [--platform <goos_goarch>]... [--checksums] <dir> <component-version>"

PLATFORMS=""
CHECKSUMS=0
DIR=""
VERSION=""
positional=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --platform)
      [[ $# -ge 2 ]] || fail "--platform takes a <goos>_<goarch> value; $usage"
      # Checked before it is spliced into an asset name below: a platform
      # carrying anything else would ask for a file no release publishes.
      [[ "$2" =~ ^[a-z0-9]+_[a-z0-9]+$ ]] || fail "'$2' is not a <goos>_<goarch> platform"
      PLATFORMS+="$2"$'\n'
      shift 2
      ;;
    --checksums) CHECKSUMS=1; shift ;;
    -*) fail "unknown option '$1'; $usage" ;;
    *)
      positional=$((positional + 1))
      case "$positional" in
        1) DIR="$1" ;;
        2) VERSION="$1" ;;
        *) fail "too many arguments; $usage" ;;
      esac
      shift
      ;;
  esac
done

[[ "$positional" -eq 2 ]] || fail "$usage"
[[ -n "$PLATFORMS" ]] || PLATFORMS=$'darwin_arm64\ndarwin_amd64\n'

# The component version names every asset, so a value that is not semver is a
# set of names no client resolves.
[[ "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] \
  || fail "'$VERSION' is not a component version (X.Y.Z[-prerelease])"
# Info.plist carries the version or its core; see the header.
CORE="${VERSION%%-*}"

[[ -d "$DIR" ]] || fail "no directory '$DIR'; it is where the build stages the renamed bundle"

# Reported before the names, because a directory here means the staging step
# copied something other than the two assets and the name checks would then
# describe half of what is wrong.
nested=$(find "$DIR" -mindepth 2 -print)
[[ -z "$nested" ]] || fail "'$DIR' contains nested entries, which a release's asset list never carries:"$'\n'"$nested"

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

# The value of an XML plist's CFBundleShortVersionString, empty when the file is
# not XML. plutil reads a binary plist as well, and is asked second so that the
# check works on a machine without it.
plist_version() {
  local text="$1" got tmp
  got=$(printf '%s\n' "$text" | awk '
    /<key>CFBundleShortVersionString<\/key>/ { want = 1; next }
    want && match($0, /<string>[^<]*<\/string>/) {
      print substr($0, RSTART + 8, RLENGTH - 17); exit
    }')
  if [[ -z "$got" ]] && command -v plutil >/dev/null 2>&1; then
    tmp=$(mktemp)
    printf '%s\n' "$text" > "$tmp"
    got=$(plutil -extract CFBundleShortVersionString raw -o - "$tmp" 2>/dev/null || true)
    rm -f "$tmp"
  fi
  printf '%s' "$got"
}

check_archive() {
  local path="$1" name="$2" list member plist got

  [[ ! -L "$path" ]] || fail "'$name' is a symlink; \`gh release upload\` would upload whatever it points at"
  [[ -f "$path" ]] || fail "no '$name'; the bill of materials resolves the desktop component by that exact name, and a release missing it reads as one that never built the app"
  [[ -s "$path" ]] || fail "'$name' is empty"
  gzip -t "$path" 2>/dev/null \
    || fail "'$name' is not a gzip file; the updater unpacks it as a gzip tar"

  list=$(tar -tzf "$path") || fail "'$name' is not a readable tar"
  member=$(printf '%s\n' "$list" | grep -E '(^|/)Tumika\.app/Contents/Info\.plist$' || true)
  member="${member%%$'\n'*}"
  [[ -n "$member" ]] \
    || fail "'$name' contains no Tumika.app/Contents/Info.plist; the updater replaces an app bundle with what this archive holds"

  # To stdout, and with the member name behind `--`: the archive is built by the
  # job that runs the branch's own code, and tar reads an operand beginning with
  # a dash as an option — `--to-command` among them — wherever it appears. Nothing
  # here is extracted onto the filesystem, so a member path reaching outside the
  # directory or through a symlink has nowhere to land.
  plist=$(tar -xzOf "$path" -- "$member") || fail "could not read $member out of '$name'"
  got=$(plist_version "$plist")
  if [[ -z "$got" ]]; then
    ok "$name — gzip tar carrying $member, whose CFBundleShortVersionString no tool here could read and which is therefore not asserted"
    return
  fi
  [[ "$got" == "$VERSION" || "$got" == "$CORE" ]] \
    || fail "'$name' reports CFBundleShortVersionString $got, not $VERSION; the asset name says the app is $VERSION, so either release.yaml never reached tauri.conf.json or the archive is from another build"
  ok "$name — gzip tar whose bundle reports $got"
}

check_signature() {
  local path="$1" name="$2" contents trimmed decoded text second

  [[ ! -L "$path" ]] || fail "'$name' is a symlink; \`gh release upload\` would upload whatever it points at"
  [[ -f "$path" ]] || fail "no '$name'; the bill of materials carries its text, and bomgen refuses an archive published without it"
  contents=$(cat "$path")
  trimmed=$(printf '%s' "$contents" | tr -d '[:space:]')
  [[ -n "$trimmed" ]] \
    || fail "'$name' is empty; a blank signature is one the updater rejects on the user's machine rather than here"

  # Tauri writes the base64 of the minisign document; plain minisign text is
  # what minisign itself writes. Either is a signature, and neither is a stub.
  decoded=$(printf '%s' "$trimmed" | openssl base64 -d -A 2>/dev/null || true)
  case "$decoded" in
    "untrusted comment:"*) text="$decoded" ;;
    *)
      case "$contents" in
        "untrusted comment:"*) text="$contents" ;;
        *) fail "'$name' is not a minisign signature: neither it nor its base64 decoding begins with an untrusted comment line" ;;
      esac
      ;;
  esac

  second=$(printf '%s\n' "$text" | sed -n '2p')
  [[ "$second" =~ ^[A-Za-z0-9+/=]+$ && ${#second} -ge 40 ]] \
    || fail "'$name' carries a comment line and no signature after it; the updater has nothing to verify the download against"
  ok "$name — a minisign signature"
}

expected=""
digests=""

while IFS= read -r platform; do
  [[ -n "$platform" ]] || continue
  archive="tumika-desktop_${VERSION}_${platform}.app.tar.gz"
  signature="${archive}.sig"
  expected+="$archive"$'\n'"$signature"$'\n'

  check_archive "$DIR/$archive" "$archive"
  check_signature "$DIR/$signature" "$signature"

  digests+="$(sha256_of "$DIR/$archive")  $archive"$'\n'
done <<< "$PLATFORMS"

# A closed set, not a filter. Every file in this directory is uploaded as a
# release asset, so anything the rules above do not name is an asset the release
# was never meant to carry — starting with Tauri's own un-renamed output.
while IFS= read -r path; do
  [[ -n "$path" ]] || continue
  name="${path##*/}"
  grep -qxF "$name" <<<"$expected" \
    || fail "'$name' is not a desktop asset for $VERSION; the staging directory becomes the release's asset list and carries nothing else"
done < <(find "$DIR" -mindepth 1 -maxdepth 1 -print)

ok "'$DIR' holds the archive and signature for each platform asked for, and nothing else"

if [[ "$CHECKSUMS" -eq 1 ]]; then
  printf '%s' "$digests"
fi

echo >&2
echo "PASS" >&2
