#!/usr/bin/env bash
# Fixture tests for verify-desktop-assets.sh.
#
# Each case stages a directory standing in for what a desktop build leg hands to
# the release, with real gzip tars and real bundle metadata, and asserts what the
# check says about it. The refusals are the point: everything downstream of the
# rename — the BOM entry, the app's own updater — resolves the desktop component
# by these names alone, and every failure here is one that would otherwise show
# up as a release the app cannot resolve.
#
# Usage: scripts/verify-desktop-assets_test.sh
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
SCRIPT="$HERE/verify-desktop-assets.sh"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

failures=0

# A minisign signature as Tauri writes it: the base64 of the minisign document.
signature_text() {
  printf 'untrusted comment: signature from tumika desktop\n'
  printf 'RWR5yaKHOTHs3PIg7F4eCiZEjhTBZ0Iv6qN0dHJ1c3RlZCBjb21tZW50Cg==\n'
  printf 'trusted comment: timestamp:1758400000\n'
  printf 'q1w2e3r4t5y6u7i8o9p0asdfghjklzxcvbnmQWERTYUIOPASDFGHJKL=\n'
}

write_signature() {
  signature_text | openssl base64 -A > "$1"
}

# A .app bundle tarred exactly as the bundler leaves it, at one version.
write_archive() {
  local path="$1" version="$2" stage
  stage="$(mktemp -d "$WORK/bundle.XXXXXX")"
  mkdir -p "$stage/Tumika.app/Contents/MacOS"
  printf 'binary\n' > "$stage/Tumika.app/Contents/MacOS/Tumika"
  cat > "$stage/Tumika.app/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0">
<dict>
  <key>CFBundleName</key>
  <string>Tumika</string>
  <key>CFBundleShortVersionString</key>
  <string>${version}</string>
  <key>CFBundleVersion</key>
  <string>${version}</string>
</dict>
</plist>
PLIST
  tar -czf "$path" -C "$stage" Tumika.app
  rm -rf "$stage"
}

CASE_DIR=""
# A fresh directory holding both platforms' assets at <component version>, with
# the bundle reporting <plist version>.
good_set() {
  local version="$1" plist_version="${2:-$1}" platform archive
  CASE_DIR="$(mktemp -d "$WORK/case.XXXXXX")"
  for platform in darwin_arm64 darwin_amd64; do
    archive="$CASE_DIR/tumika-desktop_${version}_${platform}.app.tar.gz"
    write_archive "$archive" "$plist_version"
    write_signature "$archive.sig"
  done
}

RUN_OUT=""
RUN_ERR=""
RUN_STATUS=0
run_check() {
  set +e
  RUN_OUT=$("$SCRIPT" "$@" 2>"$WORK/err")
  RUN_STATUS=$?
  set -e
  RUN_ERR=$(cat "$WORK/err")
}

report_failure() {
  echo "FAIL - $1: $2"
  printf '%s\n' "$RUN_OUT" "$RUN_ERR" | sed 's/^/       /'
  failures=$((failures + 1))
}

expect_pass() {
  local case="$1"
  if [[ "$RUN_STATUS" -ne 0 ]]; then
    report_failure "$case" "expected a pass, got exit $RUN_STATUS"
  else
    echo "ok   - $case"
  fi
}

expect_fail() {
  local case="$1" want="$2"
  if [[ "$RUN_STATUS" -eq 0 ]]; then
    report_failure "$case" "expected a failure, got exit 0"
  elif [[ "$RUN_ERR" != *"$want"* ]]; then
    report_failure "$case" "expected a message containing '$want'"
  else
    echo "ok   - $case"
  fi
}

expect_out() {
  local case="$1" want="$2"
  if [[ "$RUN_OUT" != *"$want"* ]]; then
    report_failure "$case" "expected stdout containing '$want'"
  else
    echo "ok   - $case"
  fi
}

good_set 0.1.0
run_check "$CASE_DIR" 0.1.0
expect_pass the-good-set

# A build leg stages one platform and is verified on its own; the pair is
# verified where they are attached.
good_set 0.1.0
rm "$CASE_DIR"/tumika-desktop_0.1.0_darwin_amd64.app.tar.gz*
run_check --platform darwin_arm64 "$CASE_DIR" 0.1.0
expect_pass one-platform-asked-for

good_set 0.1.0
rm "$CASE_DIR/tumika-desktop_0.1.0_darwin_amd64.app.tar.gz"
run_check "$CASE_DIR" 0.1.0
expect_fail missing-archive "no 'tumika-desktop_0.1.0_darwin_amd64.app.tar.gz'"

good_set 0.1.0
rm "$CASE_DIR/tumika-desktop_0.1.0_darwin_arm64.app.tar.gz.sig"
run_check "$CASE_DIR" 0.1.0
expect_fail missing-signature "no 'tumika-desktop_0.1.0_darwin_arm64.app.tar.gz.sig'"

good_set 0.1.0
: > "$CASE_DIR/tumika-desktop_0.1.0_darwin_arm64.app.tar.gz.sig"
run_check "$CASE_DIR" 0.1.0
expect_fail empty-signature "is empty"

good_set 0.1.0
printf 'not a signature at all\n' > "$CASE_DIR/tumika-desktop_0.1.0_darwin_amd64.app.tar.gz.sig"
run_check "$CASE_DIR" 0.1.0
expect_fail signature-of-the-wrong-shape "is not a minisign signature"

# The comment line alone, with the signature lost: a file that looks right to
# anything checking only that it is non-empty.
good_set 0.1.0
printf 'untrusted comment: signature from tumika desktop\n' \
  | openssl base64 -A > "$CASE_DIR/tumika-desktop_0.1.0_darwin_amd64.app.tar.gz.sig"
run_check "$CASE_DIR" 0.1.0
expect_fail signature-without-a-payload "nothing to verify the download against"

# minisign's own plain text, rather than Tauri's base64 of it.
good_set 0.1.0
signature_text > "$CASE_DIR/tumika-desktop_0.1.0_darwin_amd64.app.tar.gz.sig"
run_check "$CASE_DIR" 0.1.0
expect_pass plain-minisign-text

# Tauri's own output name, left un-renamed: the contract is the rename, so this
# is both a missing asset and a stray file.
good_set 0.1.0
mv "$CASE_DIR/tumika-desktop_0.1.0_darwin_amd64.app.tar.gz" "$CASE_DIR/Tumika.app.tar.gz"
run_check "$CASE_DIR" 0.1.0
expect_fail the-bundlers-own-name "no 'tumika-desktop_0.1.0_darwin_amd64.app.tar.gz'"

good_set 0.1.0
run_check "$CASE_DIR" 0.2.0
expect_fail a-version-nothing-was-built-for "no 'tumika-desktop_0.2.0_darwin_arm64.app.tar.gz'"

# The asset name and the bundle disagree: release.yaml never reached
# tauri.conf.json, and only the bundle can say so.
good_set 0.1.0 0.0.9
run_check "$CASE_DIR" 0.1.0
expect_fail a-bundle-of-another-version "reports CFBundleShortVersionString 0.0.9, not 0.1.0"

# Tauri strips a prerelease segment from CFBundleShortVersionString, so a beta
# component version is compared on its core.
good_set 0.1.0-beta.1 0.1.0
run_check "$CASE_DIR" 0.1.0-beta.1
expect_pass a-prerelease-component-version

good_set 0.1.0
printf 'x\n' > "$CASE_DIR/install-app.sh"
run_check "$CASE_DIR" 0.1.0
expect_fail stray-file "'install-app.sh' is not a desktop asset"

good_set 0.1.0
mkdir -p "$CASE_DIR/nested"
printf 'x\n' > "$CASE_DIR/nested/tumika-desktop_0.1.0_darwin_arm64.app.tar.gz"
run_check "$CASE_DIR" 0.1.0
expect_fail nested-directory "nested entries"

good_set 0.1.0
rm "$CASE_DIR/tumika-desktop_0.1.0_darwin_arm64.app.tar.gz"
ln -s /etc/passwd "$CASE_DIR/tumika-desktop_0.1.0_darwin_arm64.app.tar.gz"
run_check "$CASE_DIR" 0.1.0
expect_fail symlink "is a symlink"

good_set 0.1.0
printf 'not gzip\n' > "$CASE_DIR/tumika-desktop_0.1.0_darwin_arm64.app.tar.gz"
run_check "$CASE_DIR" 0.1.0
expect_fail not-a-gzip-file "is not a gzip file"

# An archive of something other than the app bundle: gzip, tar, and useless to
# the updater.
good_set 0.1.0
stage="$(mktemp -d "$WORK/other.XXXXXX")"
printf 'x\n' > "$stage/README"
tar -czf "$CASE_DIR/tumika-desktop_0.1.0_darwin_amd64.app.tar.gz" -C "$stage" README
rm -rf "$stage"
run_check "$CASE_DIR" 0.1.0
expect_fail no-info-plist "contains no Tumika.app/Contents/Info.plist"

# The checksums lines are what the release's checksums.txt gains, so the digest
# has to be the one of the file that gets uploaded, in goreleaser's format.
good_set 0.1.0
run_check --checksums "$CASE_DIR" 0.1.0
expect_pass digest-output-passes
if command -v sha256sum >/dev/null 2>&1; then
  want=$(sha256sum "$CASE_DIR/tumika-desktop_0.1.0_darwin_arm64.app.tar.gz" | awk '{print $1}')
else
  want=$(shasum -a 256 "$CASE_DIR/tumika-desktop_0.1.0_darwin_arm64.app.tar.gz" | awk '{print $1}')
fi
expect_out digest-output-names-the-archive "$want  tumika-desktop_0.1.0_darwin_arm64.app.tar.gz"
if [[ "$(printf '%s\n' "$RUN_OUT" | wc -l | tr -d ' ')" != 2 ]]; then
  report_failure digest-output-is-only-the-archives "expected one line per archive and nothing else"
else
  echo "ok   - digest-output-is-only-the-archives"
fi

# Without the flag, stdout is empty: the verification's own output is on stderr,
# so a redirect captures the checksums lines alone.
good_set 0.1.0
run_check "$CASE_DIR" 0.1.0
if [[ -n "$RUN_OUT" ]]; then
  report_failure no-digest-output-without-the-flag "expected an empty stdout"
else
  echo "ok   - no-digest-output-without-the-flag"
fi

run_check "$WORK/does-not-exist" 0.1.0
expect_fail missing-directory "no directory"

good_set 0.1.0
run_check "$CASE_DIR" 0.1
expect_fail not-a-component-version "is not a component version"

good_set 0.1.0
run_check --platform 'darwin/arm64' "$CASE_DIR" 0.1.0
expect_fail not-a-platform "is not a <goos>_<goarch> platform"

run_check "$CASE_DIR"
expect_fail missing-argument "usage:"

good_set 0.1.0
run_check --nonsense "$CASE_DIR" 0.1.0
expect_fail unknown-option "unknown option"

if [[ "$failures" -ne 0 ]]; then
  echo "$failures case(s) failed" >&2
  exit 1
fi
echo "all cases passed"
