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

# The binary has to match the container, which podman builds for the HOST
# architecture. Defaulting to arm64 made every assertion after `tumika install`
# fail with "exec format error" on an x86 runner — for a reason that says
# nothing about the code.
case "${TARGET_ARCH:-$(uname -m)}" in
  arm64|aarch64) ARCH=arm64 ;;
  x86_64|amd64)  ARCH=amd64 ;;
  *) fail "unsupported architecture: $(uname -m)" ;;
esac

echo "==> building the linux binary ($ARCH)"
( cd "$ROOT" && GOOS=linux GOARCH="$ARCH" CGO_ENABLED=0 \
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

echo "==> a second install actually restarts onto the new binary"
# `systemctl start` is a no-op on an active unit, so `enable --now` left the
# daemon running the OLD inode with the OLD unit settings while the command
# printed "Installed and started." Asserting is-active cannot catch that: it
# stays true precisely because nothing was restarted. The pid is the evidence.
before=$(podman exec "$NAME" systemctl show -p MainPID --value tumika.service)
podman exec "$NAME" tumika install >/dev/null 2>&1 || fail "a second install failed"
after=$(podman exec "$NAME" systemctl show -p MainPID --value tumika.service)
[[ "$(podman exec "$NAME" systemctl is-active tumika.service)" == active ]] || fail "the second install left it stopped"
[[ -n "$after" && "$after" != "0" && "$after" != "$before" ]] \
  || fail "the second install did not restart the service (pid stayed $before); an upgrade would keep running the old binary"
ok "a second install restarts onto the new binary (pid $before -> $after)"

echo "==> a system install does not use a per-user home"
# `sudo tumika install` resolves root's HOME, so without a system-wide default
# the unit named /root/.local/state/tumika — which the unprivileged service
# account cannot traverse. 203/EXEC, restart loop, and a CLI reporting "running".
# The container hides it, because InContainer() forces /var/lib/tumika, so this
# runs with detection explicitly turned OFF.
podman exec "$NAME" tumika uninstall >/dev/null 2>&1 || true
podman exec -e TUMIKA_CONTAINER=0 -e HOME=/root "$NAME" tumika install >/dev/null 2>&1 \
  || fail "install failed with container detection off"
home=$(podman exec "$NAME" sed -n 's/^Environment=TUMIKA_HOME=//p' /etc/systemd/system/tumika.service)
[[ "$home" == /var/lib/tumika ]] \
  || fail "the unit uses a per-user home ($home); the service account cannot reach it"
for _ in $(seq 1 30); do
  active=$(podman exec "$NAME" systemctl is-active tumika.service 2>/dev/null || true)
  [[ "$active" == active ]] && break
  sleep 1
done
[[ "$active" == active ]] || {
  podman exec "$NAME" journalctl -u tumika.service --no-pager -n 20 >&2
  fail "the service is $active after a non-container install"
}
ok "a system install uses $home and starts"

echo "==> status does not call a crash loop 'running'"
# A unit stuck in Restart=always reports "activating" forever and never reaches
# "failed", so mapping activating->running made a dead install report success.
podman exec "$NAME" install -m 0755 /dev/null /var/lib/tumika/bin/tumika
podman exec "$NAME" systemctl restart tumika.service >/dev/null 2>&1 || true
sleep 12
state=$(podman exec "$NAME" tumika status --json | sed -n 's/.*"state": *"\([a-z_]*\)".*/\1/p')
[[ "$state" != "running" ]] \
  || fail "a service that cannot execute its binary is reported as running"
ok "a crash-looping service is not reported as running (state=$state)"

# Put it back, so the uninstall check below runs against a healthy service.
podman cp /tmp/tumika-linux "$NAME:/var/lib/tumika/bin/tumika"
podman exec "$NAME" chown tumika:tumika /var/lib/tumika/bin/tumika
podman exec "$NAME" chmod 0755 /var/lib/tumika/bin/tumika
podman exec "$NAME" systemctl restart tumika.service >/dev/null 2>&1 || true

echo "==> uninstall leaves the data"
podman exec "$NAME" tumika uninstall >/dev/null || fail "uninstall failed"
podman exec "$NAME" test ! -f /etc/systemd/system/tumika.service || fail "the unit is still there"
podman exec "$NAME" test -f /var/lib/tumika/tumika.db || fail "uninstall deleted the database"
ok "the unit is gone and the database is not"

echo
echo "PASS"
