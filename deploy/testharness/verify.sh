#!/usr/bin/env bash
# Exercises the Linux install under a REAL systemd.
#
# The service-manager unit tests check that the right unit is rendered and the
# right commands are sequenced. They cannot tell you whether systemd ACCEPTS the
# unit, whether the service account can execute the binary, or whether the
# daemon survives a SIGKILL — and every one of those failed the first time it
# was run here, having passed every unit test.
#
# Usage: deploy/testharness/verify.sh [container-name]
set -euo pipefail

NAME="${1:-tumika-verify}"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"

fail() { echo "FAIL: $*" >&2; exit 1; }
ok()   { echo "  ok: $*"; }

echo "==> building the linux binary"
( cd "$ROOT" && GOOS=linux GOARCH="${TARGET_ARCH:-arm64}" CGO_ENABLED=0 \
    go build -o /tmp/tumika-linux ./source/cmd/tumika )

echo "==> building the harness image"
podman build -q -t tumika-harness -f "$HERE/Dockerfile" "$HERE" >/dev/null

echo "==> booting systemd"
podman rm -f "$NAME" >/dev/null 2>&1 || true
podman run -d --name "$NAME" --systemd=always --privileged tumika-harness >/dev/null
trap 'podman rm -f "$NAME" >/dev/null 2>&1 || true' EXIT

for _ in $(seq 1 30); do
  state=$(podman exec "$NAME" systemctl is-system-running 2>/dev/null || true)
  [[ "$state" == running || "$state" == degraded ]] && break
  sleep 1
done
[[ "$state" == running || "$state" == degraded ]] || fail "systemd never came up (state=$state)"
ok "systemd is up ($state)"

podman cp /tmp/tumika-linux "$NAME:/usr/local/bin/tumika"
podman exec "$NAME" chmod 0755 /usr/local/bin/tumika

echo "==> tumika install"
install_out=$(podman exec "$NAME" tumika install 2>&1) || { echo "$install_out"; fail "install returned non-zero"; }
echo "$install_out" | sed 's/^/    /'

# The account the unit runs as has to exist, or User= fails at 203/EXEC.
podman exec "$NAME" id tumika >/dev/null 2>&1 || fail "the tumika account was not created"
ok "the tumika service account exists"

# The unit has to be one systemd will actually load.
podman exec "$NAME" systemctl cat tumika.service >/dev/null || fail "systemd cannot read the unit"
ok "systemd accepted the unit"

# THE regression this harness exists for. The tree is created 0700 by root and
# the service runs unprivileged, so without an ownership handover systemd
# reports 203/EXEC and Restart=always turns it into a loop that `is-active`
# reports as "activating" forever.
owner=$(podman exec "$NAME" stat -c '%U' /var/lib/tumika/bin/tumika)
[[ "$owner" == tumika ]] || fail "the binary is owned by $owner, so the service cannot execute it"
ok "the home directory belongs to the service account"

echo "==> waiting for the service"
for _ in $(seq 1 30); do
  active=$(podman exec "$NAME" systemctl is-active tumika.service 2>/dev/null || true)
  [[ "$active" == active ]] && break
  sleep 1
done
[[ "$active" == active ]] || {
  podman exec "$NAME" journalctl -u tumika.service --no-pager -n 30 >&2
  fail "the service is $active, not active"
}
ok "the service is active"

[[ "$(podman exec "$NAME" systemctl is-enabled tumika.service)" == enabled ]] \
  || fail "the service is not enabled, so it will not survive a reboot"
ok "the service is enabled at boot"

echo "==> the API answers"
token=$(podman exec "$NAME" tumika token rotate 2>/dev/null | grep -oE 'tmk_[A-Za-z0-9_-]+' | tail -1)
[[ -n "$token" ]] || fail "could not mint an API token"
podman exec "$NAME" systemctl restart tumika.service
sleep 3
health=$(podman exec "$NAME" curl -sS --retry 5 --retry-delay 1 --retry-all-errors \
  -H "Authorization: Bearer $token" http://127.0.0.1:8737/v1/health)
echo "$health" | grep -q '"status":"ok"' || fail "health is not ok: $health"
ok "/v1/health is green through the supervised daemon"

echo "==> Restart=always recovers from a kill"
before=$(podman exec "$NAME" systemctl show -p MainPID --value tumika.service)
podman exec "$NAME" systemctl kill -s SIGKILL tumika.service
for _ in $(seq 1 30); do
  after=$(podman exec "$NAME" systemctl show -p MainPID --value tumika.service)
  [[ -n "$after" && "$after" != "0" && "$after" != "$before" ]] && break
  sleep 1
done
[[ -n "$after" && "$after" != "0" && "$after" != "$before" ]] || fail "the service did not come back (before=$before after=$after)"
[[ "$(podman exec "$NAME" systemctl is-active tumika.service)" == active ]] || fail "not active after the restart"
ok "recovered after SIGKILL (pid $before -> $after)"

echo "==> install is idempotent"
podman exec "$NAME" tumika install >/dev/null 2>&1 || fail "a second install failed"
[[ "$(podman exec "$NAME" systemctl is-active tumika.service)" == active ]] || fail "the second install left it stopped"
ok "a second install is an upgrade, not a break"

echo "==> uninstall leaves the data"
podman exec "$NAME" tumika uninstall >/dev/null || fail "uninstall failed"
podman exec "$NAME" test ! -f /etc/systemd/system/tumika.service || fail "the unit is still there"
podman exec "$NAME" test -f /var/lib/tumika/tumika.db || fail "uninstall deleted the database"
ok "the unit is gone and the database is not"

echo
echo "PASS"
