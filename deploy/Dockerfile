# The shipped image: plain and conventional, tumika as PID 1 (ADR: D9).
#
# Deliberately NOT the systemd harness in deploy/testharness — that one exists
# only to exercise `tumika install` in CI. A user running a container wants a
# container: one process, logs on stdout, and the orchestrator supervising it.
# There is no `tumika install` here and no service manager; restart policy is the
# runtime's job.

# --- build ------------------------------------------------------------------
FROM golang:1.26.6-trixie AS build

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

# --- runtime ----------------------------------------------------------------
#
# debian-slim rather than distroless or scratch, even though the binary is
# static. tumika's whole purpose is to run the vendored `claude` CLI as a
# subprocess, and that is somebody else's binary with its own expectations about
# a libc and a filesystem. An image that cannot run the provider is an image that
# cannot do the job.
#
# trixie matches what Raspberry Pi OS tracks, so the container and the Pi are not
# two different distributions.
FROM debian:trixie-slim

# ca-certificates, because every provider is reached over HTTPS: without it the
# first credential verification fails with an x509 error that reads like a bug in
# tumika.
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates \
 && rm -rf /var/lib/apt/lists/*

# A dedicated account, not root. The daemon needs no privileges: it writes only
# under its own home, and the only thing it executes is a binary it verified
# against a signed manifest.
RUN useradd --system --no-create-home --shell /usr/sbin/nologin --comment "tumika daemon" tumika \
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
#   docker run -v tumika:/var/lib/tumika tumika token rotate
#   docker run -v tumika:/var/lib/tumika -p 127.0.0.1:8737:8737 tumika \
#     serve --listen 0.0.0.0:8737
#
# The token comes first because the daemon refuses to serve without one. Run it
# against the same volume, keep the value it prints, and it is the only time that
# value exists in a form anyone can read.
ENTRYPOINT ["/usr/local/bin/tumika"]
CMD ["serve"]
