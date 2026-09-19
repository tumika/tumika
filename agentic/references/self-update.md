# Self-update

A process cannot replace the binary it is executing and keep running, so an
update is two halves either side of a restart (ADR-0003). The seam between them
is a database row, which is exactly why `update_state` is in SQLite and not in
memory.

```
apply:  fetch+verify → PRE-FLIGHT → mark pending → keep .old → rename → exit 0
boot:   ConfirmBoot → (serving) Confirm → confirmed
                    → 3 failed boots → restore .old → rolled_back → exit 0
```

Four orderings carry the whole safety property, and each is mutation-checked:

- **Pre-flight runs while the OLD binary is still in charge.** A checksum proves
  the bytes are the published ones; it does not prove they execute. A build for
  the wrong architecture hashes perfectly and exits 203 forever.
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

The boot counter increments BEFORE the attempt is judged, so a binary that dies
during startup still counts; counting after a successful start would loop
forever without ever reaching the rollback.

Disabled entirely for a development build (`buildinfo.IsDev()`) and in a
container, where the image is the unit of deployment. The API says so rather
than answering 404.

`tumika update` is one of the few commands that does NOT go through the API: the
case that matters most is a daemon that will not stay up, and an HTTP client
cannot help an operator whose service is crash-looping.
