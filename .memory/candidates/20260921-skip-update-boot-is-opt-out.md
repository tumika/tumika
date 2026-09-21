---
about: a daemon.New caller that does not set SkipUpdateBoot counts a boot attempt against a pending update; only the CLI helper opts out, so a new caller re-acquires the roll-back-from-a-CLI-process bug
saw:
  - source/daemon/internal/daemon/daemon.go
  - source/daemon/internal/cli/daemonhelper.go
  - source/daemon/internal/service/update.go
---
Couplings that hold as of 2026-09-21:
- `daemon.New` calls `UpdateService.ConfirmBoot` unless `Options.SkipUpdateBoot` is set (daemon.go). The flag is opt-out: `serve` leaves it false and `withDaemon` in cli/daemonhelper.go sets it true. Any other construction of a daemon that forgets it counts a boot against a `pending` row, and at `MaxBootAttempts` renames `<binary>.old` over the executable that process is running. Three ordinary CLI commands after `update --no-restart` were enough before the flag existed.
- `Confirm` is still called unconditionally from `ServeListener`, not gated on the flag. What stops a build that is not the pending update's from confirming is the version check inside `updateService.Confirm`: it returns `ErrNotTheUpdatedBuild` unless `state.ToVersion` equals the running component version, leaving the row `pending` and `.old` in place.
- Inverting the flag (serve opts in) needs `internal/cli/serve.go` to set it and every `internal/daemon` test that expects boot resolution to set it too.
