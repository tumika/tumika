# The shipped image: plain and conventional, tumika as PID 1 (ADR: D9).
#
# Deliberately NOT the systemd harness in deploy/testharness — that one exists
# only to exercise `tumika install` in CI. A user running a container wants a
# container: one process, logs on stdout, and the orchestrator supervising it.
# There is no `tumika install` here and no service manager; restart policy is the
# runtime's job.

# --- build (golang:1.26.6-trixie) --------------------------------------------
# Digest-pinned, like every action in .github/workflows. A moving tag means the
# same commit produces a different artifact depending on the day it is rebuilt,
# with nothing recording which base the released image actually contains.
#
# These are the multi-arch INDEX digests, taken from the registry API. The
# per-architecture digest that `podman image inspect` reports is NOT the same
# thing, and pinning one of those would break every build on the other
# architecture — including the Pi.
#
# Bumping: fetch the new index digest and update both the digest and the tag
# comment together, so the version stays readable.
FROM golang@sha256:ab563819a16cfe5faff0f96a8bb598fbb0e400ab2ac751996e60abcb23b106a3 AS build

WORKDIR /src

# Modules first, so a source-only change does not re-download the dependency
# graph on every build.
COPY go.mod go.sum ./
RUN go mod download

COPY source/ ./source/

# CGO_ENABLED=0 is a project invariant, not a container convenience: the daemon
# cross-compiles for the Pi and modernc.org/sqlite is pure Go precisely so it
# can. Setting it here means the image build is one more place that would fail
# if a cgo dependency ever crept in.
ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown
RUN CGO_ENABLED=0 go build \
      -trimpath \
      -ldflags="-s -w \
        -X main.version=${VERSION} \
        -X main.commit=${COMMIT} \
        -X main.date=${BUILD_DATE}" \
      -o /out/tumika ./source/cmd/tumika

# --- runtime (debian:trixie-slim) --------------------------------------------
#
# debian-slim rather than distroless or scratch, even though the binary is
# static. tumika's whole purpose is to run the vendored `claude` CLI as a
# subprocess, and that is somebody else's binary with its own expectations about
# a libc and a filesystem. An image that cannot run the provider is an image that
# cannot do the job.
#
# trixie matches what Raspberry Pi OS tracks, so the container and the Pi are not
# two different distributions.
FROM debian@sha256:3a39a0592364683e6bab97937b72cad5a8fa6dcbbee90edb3bb48c7f8e94f258

# ca-certificates, because every provider is reached over HTTPS: without it the
# first credential verification fails with an x509 error that reads like a bug in
# tumika.
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates \
 && rm -rf /var/lib/apt/lists/*

# A dedicated account, not root. The daemon needs no privileges: it writes only
# under its own home, and the only thing it executes is a binary it verified
# against a signed manifest.
# The uid is PINNED, not left to useradd.
#
# `useradd --system` takes whatever id is free in the base image at build time.
# The state directory is created 0700 owned by that id, and a named volume
# inherits it on first use — so if a base-image refresh or an added package
# shifted the assignment, a container built from the new image could not read a
# volume populated by the old one. The daemon would fail with EACCES on its own
# data, which is indistinguishable from the total data loss this file goes to
# some trouble elsewhere to prevent.
RUN groupadd --system --gid 10001 tumika \
 && useradd --system --uid 10001 --gid 10001 --no-create-home \
      --shell /usr/sbin/nologin --comment "tumika daemon" tumika \
 && install -d -o tumika -g tumika -m 0700 /var/lib/tumika

COPY --from=build /out/tumika /usr/local/bin/tumika

# TUMIKA_HOME is set explicitly rather than left to detection. Container
# detection is a heuristic, and a daemon that silently picked a different data
# directory after an upgrade would look like total data loss.
ENV TUMIKA_HOME=/var/lib/tumika \
    TUMIKA_CONTAINER=1

# The state directory is the whole install: database, sealed credentials, and the
# vendored provider binaries. Everything worth keeping is under here, and
# everything under here is worth keeping.
VOLUME ["/var/lib/tumika"]

USER tumika

EXPOSE 8737

# The API listens on loopback by DEFAULT, which inside a container means nothing
# outside it can connect. That is deliberate — the API carries a bearer token in
# clear text — so publishing the port also means choosing to bind wider:
#
#   docker volume create tumika
#   docker run --rm -v tumika:/var/lib/tumika tumika token rotate
#   docker run -v tumika:/var/lib/tumika -p 127.0.0.1:8737:8737 tumika \
#     serve --listen 0.0.0.0:8737
#
# The token comes first because the daemon refuses to serve without one. It is
# the only time that value exists in a form anyone can read.
#
# BOTH commands need the SAME -v. Because VOLUME is declared, omitting it still
# succeeds — against a throwaway anonymous volume — so `token rotate` appears to
# work and `serve` then fails with "no API token configured", an error pointing
# at the wrong thing entirely.
#
# A BIND MOUNT needs preparing first. A named volume inherits this directory's
# ownership; a bind mount does not, so -v /srv/tumika:/var/lib/tumika arrives
# root-owned and the unprivileged account cannot write it:
#
#   mkdir -p /srv/tumika && chown 10001:10001 /srv/tumika && chmod 700 /srv/tumika
ENTRYPOINT ["/usr/local/bin/tumika"]
CMD ["serve"]
