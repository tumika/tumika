# Service management

`tumika install` sets the daemon up under the platform's supervisor — a systemd
system unit on Linux, a LaunchAgent on macOS — and `uninstall / start / stop /
status` drive it from there. Neither driver is behind a build tag: both compile
everywhere and only `servicemgr.New` consults `runtime.GOOS`, so the Linux unit
rendering and command sequencing are testable on a Mac.

Six things the Linux install must do, every one of which was a silent failure
first — the install printed success and the service did not run:

- **Use `paths.SystemHome`, not the XDG per-user default.** `sudo tumika install`
  resolves *root's* `HOME`, so the unit named `/root/.local/state/tumika`, which
  the unprivileged service account cannot traverse.
- **Hand `TUMIKA_HOME` to the service account.** The layout is created `0700` by
  root and the unit runs as `tumika`.
- **Create the service account BEFORE probing key custody.** The handover probe
  runs a transient unit *as* that account, so probing first made `systemd-run`
  fail to resolve the user on every first install — the probe reported "no
  handover", the freshly sealed key was deleted, and the host dropped to the file
  key permanently.
- **Mint an API token before starting**, and print it as soon as it exists. The
  daemon refuses to serve without one; and the hash is stored the moment it is
  minted, so an install that fails afterwards would otherwise leave a daemon with
  a token nobody has ever seen.
- **Prove the credential handover** before writing `LoadCredentialEncrypted=`.
- **`systemctl enable` then `restart`, never `enable --now`.** `start` is a no-op
  on an active unit, so the documented upgrade path left the daemon running the
  old binary with the old unit settings.

Two things `status` must never do:

- **Report `activating` as running.** That is what a `Restart=always` crash loop
  reports *forever* — it never reaches `failed`, because the supervisor keeps
  trying. It maps to `starting`, and to `failed` once `NRestarts` is above zero.
- **Assume a LaunchAgent is enabled because a plist exists.** `launchctl
  print-disabled` is the answer.

The whitespace/`%` rule lives in the systemd driver, not in `Config.Validate`:
macOS's own default home is `~/Library/Application Support/tumika`, and refusing
a space would break every Mac install to protect Linux.

`deploy/testharness/verify.sh` runs the whole thing under a real systemd in a
podman container: install, ownership, the unit systemd actually accepts,
`/v1/health` through the supervised daemon, `Restart=always` recovery from
`SIGKILL`, a second install that must land on a **new pid**, a system install
with container detection **off**, a crash loop that must not report as running,
and an uninstall that leaves the database. Unit tests check what is rendered and
sequenced; only this checks whether systemd agrees.

Note what the container itself hides: `InContainer()` forces the system home, so
the per-user-home bug was invisible until the harness started running an install
with `TUMIKA_CONTAINER=0`. A harness has blind spots of its own, and they are
worth writing down when found.
