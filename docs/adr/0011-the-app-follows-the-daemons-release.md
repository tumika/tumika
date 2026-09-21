---
status: accepted
date: 2026-09-21
---

# The desktop app follows the release its daemon runs

ADR-0006 defines a release as a label over components that each keep their own semver,
ADR-0009 how the signed documents are published, and ADR-0010 how the macOS app becomes a
component of a release. This records how the app decides which component version to run, what
it trusts to decide, and how the installer arrives at the same answer.

## Decisions

- **The app runs the component version its daemon's release names, in both directions.** The
  daemon is the unit a user chooses a release for, and the app is a client of it (ADR-0005), so
  the app's desktop component version is whatever the bill of materials of the daemon's release
  names. Any component version that differs from the running one is an update; nothing compares
  versions for order. A daemon that moves to an older release pulls the app back with it, which
  is the point: an app newer than its daemon speaks to an API that release does not have.
  Rejected: updating only to a higher component version, which leaves the app ahead of a daemon
  that was rolled back. Equality is "already paired", and a release with no desktop component,
  no asset for this architecture, or an asset without a usable signature is "nothing to do"
  with a reason the popover shows, never an error.

- **The app trusts four things, in this order.**
  1. The daemon's `GET /v1/version`, behind the API token the app reads from the login Keychain,
     for the release label. The label is validated against the strict release-label pattern
     before it reaches a URL; the pattern is a copy of the daemon's, and a test parses the
     daemon's source to keep them equal.
  2. The release's bill of materials and its detached ECDSA P-256 signature, fetched from
     `get.tumika.org` and verified against a compiled-in key list. The app carries its own copy
     of the daemon's list (`platform/release/keys.go`); a test fails when the two differ. The
     document is read only after it verifies, and it must describe the release that was asked
     for.
  3. The pairing decision taken from that document.
  4. The Tauri updater verifying the downloaded archive against the minisign public key committed
     in `tauri.conf.json`.
  The two signatures are independent: the release key vouches for which archive the release
  names, the updater key for the bytes of the archive. Compromising one does not yield the other.
  The app installs code, which is why it verifies the document itself rather than relying on the
  archive signature alone: an archive signature says the bytes are genuine, not that they belong
  to the daemon's release.

- **The decision reaches the updater through a one-shot loopback endpoint.** The plugin decides
  from a manifest its endpoint serves. The app serves the decided `{version, url, signature}` from
  an ephemeral `127.0.0.1` port for a single request, builds the updater with that endpoint and a
  comparator that accepts any differing component version, and lets the plugin download, verify
  and install. The body holds a published URL and a published signature, no secret. The plugin
  refuses a non-https endpoint unless `plugins.updater.dangerousInsecureTransportProtocol` is
  `true`, so the flag is committed. It governs endpoint schemes only; the archive download stays
  on the https URL the document gives, and its signature check is unaffected. The flag exists for
  the loopback endpoint and for nothing else.
  Rejected: publishing a Tauri-shaped manifest on `get.tumika.org` for the updater to fetch. It
  would be a second unauthenticated fetch from the host whose trustworthiness the signed document
  exists to remove (ADR-0010), and it could not express the daemon's release, which the host
  does not know.

- **`require_signed_version` stays off.** The plugin flag would require the archive's signature
  to carry the component version it was signed for. The version the updater is told comes from
  the document the app has already verified, not from an unauthenticated endpoint, so the flag
  guards a substitution that cannot occur here, and it would reject any archive whose signature
  records no version.

- **The installer reaches the same answer with the same checks.** `scripts/install-app.sh`,
  served from `get.tumika.org` beside `install-daemon.sh`, refuses on any system but macOS and
  points at the daemon installer. It asks the daemon which release to install, reading the token
  from the Keychain, and hands it to curl on stdin, never in argv. `TUMIKA_RELEASE` replaces
  the question for an install with no daemon running. The label is checked against the strict
  pattern before it forms a URL; the document's signature is verified with `openssl` against an
  embedded copy of the release key, kept equal to the daemon's by a test; the document must
  describe the requested release; the archive's SHA-256 must match the document's; and the
  archive is listed before it is unpacked, refusing an absolute path, a `..`, or anything but
  `Tumika.app`. The app lands in `~/Applications`, replaced by rename, with no sudo and never
  `/Applications`. The asset's Tauri `signature` field belongs to the updater and the installer
  does not read it. curl sets no quarantine attribute, so an ad-hoc signed bundle opens without a
  Gatekeeper prompt. There is no fallback to a channel head: an app not paired with its daemon
  would update itself away or refuse to, so an unreachable daemon is an error.
  Rejected: installing the desktop entry of a channel head. The daemon may follow a different
  release than the channel's head.

## Consequences

- Two reqwest majors link into the app: the app's own client (0.12) and the updater plugin's
  (0.13). They converge when the app moves to the updater's major.
- The daemon's URL is a compile-time constant, `http://127.0.0.1:8737`, so the app pairs with a
  daemon on the same machine only. A remote daemon is GitHub issue #48; more than one user per
  machine is issue #54.
- Losing the updater private key strands installed apps on the old public key (ADR-0010).
- An app cannot update until a release that changes the desktop component has been published
  with its archive signed.
