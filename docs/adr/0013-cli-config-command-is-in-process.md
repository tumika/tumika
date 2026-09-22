---
status: accepted
date: 2026-09-22
---

# `config set`/`reset` are the CLI's one in-process exception, narrowing ADR-0004

ADR-0004 makes the CLI an HTTP client of the daemon so that one API surface carries every
business rule, for a local or a remote daemon alike. That holds for every command except the
one an operator reaches for when the daemon is *not* answering: a fresh install with no token
yet, a crash-looping daemon, or a setting whose current value is the reason the daemon will not
serve — `update.auto_apply` set in a way that makes every boot apply an update that fails, for
instance. An HTTP client has no daemon to call in any of those cases, so the rule that keeps
the CLI thin would also make the failure unrecoverable without editing the database by hand.

## Decisions

- **In-process CLI access is allowed only for a command that must work without a serving
  daemon.** This is the narrow exception ADR-0004's decision bullet now points at, not a general
  license — every other command stays an HTTP client, including `config list`/`get`, which have
  no such requirement and gain nothing from bypassing the API.

- **`tumika config set` and `tumika config reset` qualify.** They are how an operator escapes a
  bad setting without the daemon serving: run `withDaemon`, open the database directly, and call
  `ConfigService.Set`/`Reset` in-process — the same service method the API handler calls, so the
  validation and the sentinel errors are identical regardless of which entry point reached them.

- **`config list` and `config get` also run through `withDaemon`, in-process, alongside `set`
  and `reset`.** They read no differently whether or not a daemon is serving, and splitting the
  four subcommands across two access paths — HTTP when a daemon happens to be up, in-process
  otherwise — would make `config`'s behavior depend on a runtime condition the operator cannot
  see from the command line.

- **The service, not the CLI, owns validation.** `config set` converts a plain shell value to
  the JSON shape its key's `Kind` calls for and calls `ConfigService.Set`; a bad value surfaces
  as `ErrInvalidSetting`/`ErrUnknownSetting` from the service, mapped to an exit code the same
  way every other CLI command maps a service sentinel error. The CLI does not duplicate that
  validation locally.

## Considered alternatives

- **Leaving `config set`/`reset` as HTTP-only, like every other command.** Rejected: it removes
  exactly the recovery path the command exists for. An operator who set `server.listen` to an
  address nothing binds to, or `update.auto_apply` to a value that update-loops the daemon, has
  no running API to call `PUT /v1/config` against.
- **A separate `tumika repair` or `tumika db` command outside the `config` namespace.**
  Rejected: it duplicates `ConfigService`'s validation and key set under a different name, and
  gives the operator two commands to remember for the same underlying operation depending on
  whether the daemon happens to be up.
- **Widening the exception to "any CLI command may go in-process when convenient."** Rejected:
  that is the general license ADR-0004 already rejected, for the reasons recorded there — two
  entry points for the same rule, and losing the remote-daemon case for free. This ADR narrows
  the prohibition by one specific, bounded case; it does not remove it.

## Consequences

- `source/daemon/internal/cli/config.go` opens the database directly through `withDaemon`, the
  same helper `token rotate` and `update` already use for their own in-process needs, rather
  than issuing an HTTP request.
- A future command has to make the same case this one does — that it must work without a
  serving daemon — before it may follow the same path. Convenience alone does not qualify.
- `config set`/`reset` print a note after a successful write: a running daemon picks the change
  up on its next read, so a value like `server.listen` that only takes effect at
  `server.listen` time needs a restart regardless of which entry point wrote it.
