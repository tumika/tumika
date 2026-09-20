# Self-update

A process cannot replace the binary it is executing and keep running, so an
update is two halves either side of a restart (ADR-0003). The seam between them
is a database row, which is exactly why `update_state` is in SQLite and not in
memory.

```
apply:  head+rule → fetch+verify → PRE-FLIGHT → mark pending → keep .old → rename → exit 0
boot:   ConfirmBoot → (serving) Confirm → confirmed
                    → 3 failed boots → restore .old → rolled_back → exit 0
```

Four orderings carry the whole safety property, and each is mutation-checked:

- **Pre-flight runs while the OLD binary is still in charge.** A checksum proves
  the bytes are the published ones; it does not prove they execute. A build for
  the wrong architecture hashes perfectly and exits 203 forever. The staged binary
  runs `version`, whose reported component version must equal the one requested,
  then `version --json`, whose `schema_version` must not be lower than the
  database's. Both refusals happen before the `pending` row and before any
  rename, so the running binary is untouched.
- **`pending` is recorded BEFORE the replacement.** A crash between the two
  leaves a record against a binary that was never swapped, which `ConfirmBoot`
  resolves harmlessly. The reverse leaves a swapped binary with no record, and
  nothing would ever roll it back.
- **The old binary is kept, not overwritten.** It is the only thing a rollback
  can restore from, and the boot after a failed update is precisely when the
  network cannot be assumed to work.
- **`Confirm` runs once the daemon is SERVING**, not merely constructed — and
  only then deletes `.old`. A binary that starts and then fails every request has
  proven nothing.

## Channels and the update rule

The daemon follows the channel in `update.channel` (`stable`, `beta`, `edge`).
Channels are cumulative and the head is the most recently published release the
channel receives (ADR-0007). A head replaces the running build when:

- **stable, beta:** it was published later AND its component version is
  semver-greater. Never an automatic downgrade.
- **edge:** it was published later. Semver is not consulted.

`supersedes` in `service/update.go` is the one rule; `Check` and `Apply` both
call it, so an edge downgrade that `Check` offers is not refused by `Apply`. In
the apply ordering it runs first, after the head is re-read: the requested
component version must equal the head's, or `Apply` refuses with a conflict.

The running build's publication time comes from its own
`/releases/<label>.json`. A development build (release `dev`) counts as older.
Any failure other than a 404 — an unverifiable signature, an unreachable host —
stops the check, because reading it as "older" could downgrade an edge daemon on
the strength of a document nobody verified.

A 404 falls back to the **watermark**: the publication time `Apply` recorded in
`update_state.to_published_at` alongside the version it installed. It counts
only when it belongs to the running build — the row's `to_version` is the
running component version and its status is `pending` or `confirmed`; a
`rolled_back` row's watermark dates a build that is not running. With no
watermark for the running build, a 404 counts as older, so a first install and a
daemon whose pruned edge release predates its own watermark are still offered
the head.

Without that floor an edge daemon can be rolled backwards with no forged
signature at all: edge decides on recency alone, and a host that withholds the
running build's own document — indistinguishable from a legitimate prune — while
replaying a genuine, correctly signed, older channel head dates the daemon at
nothing, and the older release supersedes it. The watermark is written by the
daemon itself, from the head it is installing, before the binary is swapped, so
nothing the host serves can lower it. A bill of materials that reads cleanly is
authoritative and leaves the watermark alone, which keeps `Check` a read. The
watermark is read and written through its own repository methods rather than
alongside the state machine's columns, because `ConfirmBoot` runs before
`Migrate` and everything it reads has to exist on the previous schema.

## Trust chain

The daemon reads `<base>/channels/<channel>.json`, which names the head release
and its assets, and `<base>/releases/<label>.json` for its own release's
publication time. Each has a detached signature beside it (`<document>.sig`):

- The signature is ECDSA P-256 over the SHA-256 of the exact bytes. The body is
  parsed only after it verifies.
- Verification is against a compiled-in LIST of public keys
  (`platform/release/keys.go`); any listed key may verify, which is what makes
  rotation possible.
- It fails closed. A 404 on a document means no release (`ErrNoRelease`); a 404
  on its signature means it is refused as unsigned. Tampered, unknown-signer and
  malformed documents are refused.
- The asset's `sha256` comes from the signed BOM and is enforced on the download;
  the file is written only if it matches.
- The release label from the daemon's own build stamp is validated against a
  strict pattern before it is placed in a URL.

The documents are published to `https://get.tumika.org` by `publish-pages.yml`,
generated from the Releases API and signed with the release key whose public half
is in `keys.go` (ADR-0009). `scripts/install-daemon.sh` follows the same chain for
a first install: it verifies the BOM's signature with `openssl` against an
embedded copy of that key before reading anything out of it.

The boot counter increments BEFORE the attempt is judged, so a binary that dies
during startup still counts; counting after a successful start would loop
forever without ever reaching the rollback.

Disabled entirely for a development build (`buildinfo.IsDev()`) and in a
container, where the image is the unit of deployment. The API says so rather
than answering 404.

`tumika update` is one of the few commands that does NOT go through the API: the
case that matters most is a daemon that will not stay up, and an HTTP client
cannot help an operator whose service is crash-looping.
