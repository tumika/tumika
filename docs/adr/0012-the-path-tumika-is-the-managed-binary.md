---
status: accepted
date: 2026-09-21
---

# The `tumika` on PATH is the managed binary, and `update` restarts the service onto it

ADR-0003 puts the daemon's binary in its own home and updates it by atomic rename plus a clean
exit. That leaves two gaps: the `tumika` an operator types can be a different file from the
one the service runs, and a replaced binary is not running until something restarts the
service. This records how the PATH copy, the update command and the boot confirmation close them.

## Decisions

- **On macOS, `install` links the invoked PATH copy to the managed binary.** When the
  regular-file `tumika` that PATH resolves to is the one being invoked, it is replaced by a
  symlink to the managed binary, through a temporary symlink and a rename so PATH never lacks a
  `tumika`. It does nothing when the copy already is the managed binary or a link to it. A copy
  invoked from anywhere else is left alone with a note. `--binary` links nothing. A PATH
  directory that cannot be written is a warning, not a failure. After `uninstall` with a deleted
  home the link dangles; that is accepted.

- **On Linux, `install` links nothing and prints a note.** The managed directory is `0700` and
  owned by the service account, so a link into it is unusable to the operator, and
  `sudo <absolute path> install` is never the PATH `tumika`.
  Rejected: a link in `/usr/local/bin`. It is root-owned, dangling for every non-service user,
  and reintroduces the privileged host configuration ADR-0003 avoids.

- **`update.auto_apply` defaults to true.** An install whose key is unset applies updates at its
  next check. The desktop app follows its daemon's release (ADR-0011), so an auto-applied daemon
  update moves the app too.

- **`tumika update` restarts the service by default** when it is installed and running: Stop,
  then Start. `--no-restart` skips it. When the replaced binary is not the managed copy, it
  warns (`tumika install`) and does not restart, since the service would relaunch the build it
  already runs. Stop succeeding and Start failing exits non-zero.
  Rejected: making `update` follow the service's managed binary instead of the invoked one; the
  command replaces what the operator ran and says so when that is not what the service runs.
  `syscall.Exec` was rejected in ADR-0003 and is not revisited.

- **Only `serve` confirms a boot.** CLI processes open the daemon with
  `daemon.Options.SkipUpdateBoot`, so a `tumika version` cannot confirm an update it merely
  ran beside. `Confirm` additionally refuses unless the running component version equals the
  pending row's `to_version` (`service.ErrNotTheUpdatedBuild`), leaving the row pending and
  `.old` kept.

## Consequences

- A newer CLI run against an older running daemon can migrate the schema, so the old daemon can
  crash-loop on the schema-version guard until `install` is re-run.
