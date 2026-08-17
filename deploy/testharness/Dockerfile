# CI-only harness: a real systemd, so `tumika install` is EXERCISED rather than
# asserted about.
#
# The service-manager code renders a unit and sequences systemctl calls, and unit
# tests can check both without systemd — but they cannot tell you whether systemd
# ACCEPTS the unit. A directive with a typo, an option a given systemd version
# does not know, a sandbox setting that blocks the daemon's own data directory:
# every one of those passes a rendering test and fails on the Pi.
#
# Debian trixie (trixie-slim), because that is what Raspberry Pi OS tracks.
# Digest-pinned like the shipped image, and for a second reason here: this
# builds from whatever `debian:trixie-slim` means in the LOCAL image store, so a
# stray `podman pull --platform linux/amd64` on a developer's machine silently
# rebuilds the harness on the wrong architecture. The install then fails in ways
# that look like bugs in tumika. The multi-arch index digest resolves correctly
# on every host.
FROM debian@sha256:34cd9e9fd437c0a095ec39cb2e73422c9f30821b0d0848ed74fd0d43bae4d958

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
