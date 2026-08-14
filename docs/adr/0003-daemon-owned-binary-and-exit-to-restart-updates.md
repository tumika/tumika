---
status: accepted
date: 2026-08-14
---

# The daemon owns its own binary, and updates itself by atomic rename plus a clean exit

tumika self-updates. On Linux it runs as an unprivileged `tumika` system user under a systemd
system unit; on macOS it runs as a LaunchAgent in the operator's session. In neither case does
the running process have root.

That makes the conventional layout unworkable. A binary at `/usr/local/bin/tumika` is owned by
root, so a non-root process cannot replace it. And `systemctl restart tumika` from a non-root
user needs a polkit rule — one more piece of privileged host configuration to install, explain
and get wrong.

We decided to **relocate the binary into the daemon's own data directory** and to **replace
restart with exit**.

## Decisions

- **The real binary lives at `/var/lib/tumika/bin/tumika`**, owned by the `tumika` user. A
  symlink at `/usr/local/bin/tumika` keeps the CLI on the operator's `PATH`. The symlink is
  installed once, with the privileges the installer already has; it is never rewritten by an
  update.
- **An update is: stage → verify → atomic rename → `os.Exit(0)`.** The staged file is written
  in the *target's own directory* so the rename cannot cross a filesystem boundary. The service
  manager's `Restart=always` (systemd) / `KeepAlive` (launchd) relaunches the process, which
  starts from the new binary. No privilege, no polkit, no restart command.
- **`Restart=always`, not `on-failure`.** A clean `os.Exit(0)` is precisely the case
  `on-failure` refuses to restart, so `on-failure` would turn every successful update into a
  permanent stop.
- **The previous binary is kept as `.old`** next to the new one.
- **Boot is confirmed, and failure rolls back.** Before replacing, `update_state` is written as
  `pending` with `boot_attempts = 0`. Each start increments it; at three attempts the daemon
  restores `.old` and marks `rolled_back`. On a successful serve, the state becomes `confirmed`
  and `.old` is deleted.
- **A pre-flight runs while the old binary is still in charge:** exec `<staged> version` with a
  10-second timeout and assert the semver matches what was fetched. A corrupt or wrong-arch
  artifact is caught before it is ever the live binary.
- **Self-update disables itself in containers**, and when `version == "dev"`. The image is the
  unit of deployment; a container that rewrites itself no longer matches its tag.

## Considered alternatives

- **Binary in `/usr/local/bin`, updated with a privileged helper.** Rejected: it means shipping
  a setuid helper or a polkit policy, which is a large increase in attack surface and installer
  complexity for one operation.
- **`systemctl restart` after replacing.** Rejected: needs polkit for a non-root caller, and it
  is redundant — the supervisor is already configured to restart the process.
- **`syscall.Exec` to re-exec in place.** Rejected: it keeps the process ID and the inherited
  file descriptors, so a leaked descriptor or a wedged listener survives the "restart". Exiting
  and being relaunched gives a genuinely fresh process, which is what the update is supposed to
  produce.
- **A separate updater binary or a package manager.** Rejected: two artifacts to release and
  sign, and a package manager is not available on every target.

## Consequences

- **The binary is not root-owned.** A compromise of the `tumika` user is a compromise of the
  binary it will next execute. This is the accepted cost, and it is bounded by the same user
  already owning the database, the sealed credentials and the vendored `claude`. The mitigation
  is at the fetch boundary — checksum verification today, a signed `checksums.txt` before any
  public release.
- Because updates rely on the supervisor, **an update while running outside a supervisor exits
  and does not come back.** The CLI must say so plainly when it is not running under one.
- `.old` means two copies of the binary on disk between an update and its confirmation.
- The three-attempt rollback counter is per-boot, so a crash loop caused by something other
  than the new binary (a corrupt database, a missing key) also triggers a rollback. That is
  accepted: rolling back is the safer response to "the new version cannot boot", whatever the
  cause, and the previous binary meeting a newer schema is refused loudly by the schema-version
  guard rather than silently.
- `tumika update` and `UpdateRunner` share one code path, so the manual and automatic routes
  cannot diverge.
