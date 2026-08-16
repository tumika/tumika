# CI-only harness: a real systemd, so `tumika install` is EXERCISED rather than
# asserted about.
#
# The service-manager code renders a unit and sequences systemctl calls, and unit
# tests can check both without systemd — but they cannot tell you whether systemd
# ACCEPTS the unit. A directive with a typo, an option a given systemd version
# does not know, a sandbox setting that blocks the daemon's own data directory:
# every one of those passes a rendering test and fails on the Pi.
#
# Debian trixie, because that is what Raspberry Pi OS tracks.
FROM debian:trixie-slim

ENV DEBIAN_FRONTEND=noninteractive
ENV container=podman

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      systemd systemd-sysv dbus ca-certificates procps curl \
 && rm -rf /var/lib/apt/lists/*

# Strip the units a container has no use for. Without this, systemd spends the
# boot failing at things that have nothing to do with tumika, and the journal is
# too noisy to diagnose the thing under test.
RUN find /etc/systemd/system /lib/systemd/system \
      -path '*.wants/*' \
      \( -name '*getty*' -o -name '*udev*' -o -name '*mount*' \
         -o -name '*swap*' -o -name '*timesyncd*' -o -name '*resolved*' \) \
      -exec rm -f {} + 2>/dev/null || true

STOPSIGNAL SIGRTMIN+3
CMD ["/sbin/init"]
