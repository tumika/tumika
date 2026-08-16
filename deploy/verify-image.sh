#!/usr/bin/env bash
# Exercises the SHIPPED image the way a user would.
#
# Building the image proves the Dockerfile parses. It does not prove the image can
# do anything: that the binary runs on that base, that an unprivileged account
# can write the volume, that the two-step first run works, or that the API
# answers. Every one of those is a way to publish an image nobody can use.
#
# Usage: deploy/verify-image.sh [image-tag]
set -euo pipefail

# Either engine works here: nothing in this script needs systemd, unlike the
# install harness. Detected rather than assumed, so a runner with only docker is
# not a reason for the image gate to fail.
ENGINE="${CONTAINER_ENGINE:-}"
if [[ -z "$ENGINE" ]]; then
  for candidate in podman docker; do
    if command -v "$candidate" >/dev/null 2>&1; then ENGINE="$candidate"; break; fi
  done
fi
[[ -n "$ENGINE" ]] || { echo "FAIL: no podman or docker on PATH" >&2; exit 1; }

IMAGE="${1:-tumika:verify}"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/.." && pwd)"
VOLUME="tumika-verify-data"
NAME="tumika-verify"
PORT="18737"

fail() { echo "FAIL: $*" >&2; exit 1; }
ok()   { echo "  ok: $*"; }

cleanup() {
  $ENGINE rm -f "$NAME" >/dev/null 2>&1 || true
  $ENGINE volume rm -f "$VOLUME" >/dev/null 2>&1 || true
}
trap cleanup EXIT
cleanup

echo "==> building the shipped image"
$ENGINE build -q -t "$IMAGE" -f "$HERE/Dockerfile" "$ROOT" >/dev/null

echo "==> the binary runs on this base"
version=$($ENGINE run --rm "$IMAGE" version) || fail "the image cannot run tumika"
echo "$version" | grep -q "^tumika " || fail "unexpected version output: $version"
ok "$(echo "$version" | head -1)"

echo "==> it does not run as root"
id_out=$($ENGINE run --rm --entrypoint id "$IMAGE")
echo "$id_out" | grep -q "uid=0(root)" && fail "the image runs as root: $id_out"
ok "runs as ${id_out}"

$ENGINE volume create "$VOLUME" >/dev/null

echo "==> the first run mints a token against the volume"
# The daemon refuses to serve without one, so this is the documented first step
# and it has to work against a fresh volume — an unprivileged account writing a
# directory the image created at build time.
token=$($ENGINE run --rm -v "$VOLUME:/var/lib/tumika" "$IMAGE" token rotate \
  | grep -oE 'tmk_[A-Za-z0-9_-]+' | tail -1)
[[ -n "$token" ]] || fail "could not mint a token against the volume"
ok "minted a token"

echo "==> serving"
$ENGINE run -d --name "$NAME" -v "$VOLUME:/var/lib/tumika" \
  -p "127.0.0.1:$PORT:8737" "$IMAGE" serve --listen 0.0.0.0:8737 >/dev/null

for _ in $(seq 1 30); do
  health=$(curl -fsS -H "Authorization: Bearer $token" \
    "http://127.0.0.1:$PORT/v1/health" 2>/dev/null) && break
  sleep 1
done
echo "${health:-}" | grep -q '"status":"ok"' || {
  $ENGINE logs "$NAME" >&2
  fail "health is not ok: ${health:-<no response>}"
}
ok "/v1/health is green through the published port"

echo "==> binding beyond loopback is warned about"
# The API carries a bearer token in clear text. Publishing a port is a choice,
# and the daemon has to say so rather than let it pass silently.
#
# Captured into a variable rather than piped: the daemon logs to stderr, and a
# `$ENGINE logs ... | grep` pipeline reads only stdout — which is empty, so the
# check passed nothing to grep and failed for the wrong reason.
logs=$($ENGINE logs "$NAME" 2>&1)
grep -q "beyond loopback" <<<"$logs" \
  || fail "no warning was logged for binding beyond loopback"
ok "the clear-text warning is logged"

echo "==> the token is not in the logs"
if grep -q "$token" <<<"$logs"; then
  fail "the API token appears in the container logs"
fi
ok "the token stays out of the logs"

echo "==> state survives a restart"
# The volume is the whole install. A token that stopped working after a restart
# would mean the database was inside the container layer.
$ENGINE restart "$NAME" >/dev/null
for _ in $(seq 1 30); do
  health=$(curl -fsS -H "Authorization: Bearer $token" \
    "http://127.0.0.1:$PORT/v1/health" 2>/dev/null) && break
  sleep 1
done
echo "${health:-}" | grep -q '"status":"ok"' \
  || fail "the same token stopped working after a restart; state is not on the volume"
ok "the same token still works after a restart"

echo
echo "PASS"
