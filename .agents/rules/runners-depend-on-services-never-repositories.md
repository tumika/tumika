# A runner depends on services only, and never on a repository or a database handle

A **runner** is a supervised long-lived process:

```go
type Runner interface {
    Name() string
    Start(ctx context.Context) error
    Stop(ctx context.Context) error
}
```

It is constructed in `daemon` and handed **services**. It is never handed a repository, a
`*sql.DB`, or a `Sealer`. If a runner needs to read or write persistent state, the service that
owns that state exposes a method for it.

Runners are the layer most likely to erode, because they are the one place where the pressure
to "just do the query in the loop" is strongest: a runner has a ticker, no HTTP request, no
caller waiting, and it feels like infrastructure rather than product code. It is not. A runner
is a *caller* — structurally identical to an HTTP handler, differing only in what wakes it up.

This rule exists mostly to pre-empt stage 2. `WorkflowRunner` will want to claim a due run,
mark it started, execute it, and record the result — four writes in a tight loop, all of them
tempting to issue directly. Every one of them is a business rule (what "due" means, what a
claim is, whether a crashed run is retried), and every one belongs to a service.

## Applies to

| | |
|---|---|
| `source/internal/runner/**` | the rule binds here |
| `UpdateRunner` | calls `UpdateService.Check` / `Apply`, not `UpdateStateRepository` |
| `CredentialMonitorRunner` | calls `ProviderService.VerifyCredential`, not `CredentialRepository.UpdateStatus` |
| `source/internal/daemon/**` | wires runners; the only place the dependency could be passed |
| `.golangci.yml` (`depguard`) | mechanical enforcement: `runner` may not import `repository` |

## Example

**WRONG** — the runner owns the "expired" rule and the write:

```go
func (r *CredentialMonitorRunner) tick(ctx context.Context) {
    creds, _ := r.credRepo.ListActive(ctx)
    for _, c := range creds {
        if c.ExpiresAt != nil && time.Until(*c.ExpiresAt) < 7*24*time.Hour {
            _ = r.credRepo.UpdateStatus(ctx, c.ID, "expiring", "")
        }
    }
}
```

Nothing here can be tested without a database, the seven-day threshold now exists in a second
place, and `/v1/health` — which reads status through `ProviderService` — will report whatever
this loop happened to write, with no shared definition of what the statuses mean.

**RIGHT** — the runner supplies only the schedule:

```go
func (r *CredentialMonitorRunner) tick(ctx context.Context) {
    for _, id := range r.providers.EnabledIDs(ctx) {
        if _, err := r.providers.VerifyCredential(ctx, id); err != nil {
            r.log.WarnContext(ctx, "credential verification failed",
                "provider", id, "err", err)   // never the secret — see never-log-or-return-a-credential-secret.md
        }
    }
}
```

## Why

Runners also have an obligation the other callers do not: `Start` must return when `ctx` is
cancelled, and `Stop` must be idempotent and safe to call after a failed `Start`. Keeping them
free of data access is what makes that tractable — a runner that owns no transaction has
nothing to unwind on shutdown, so the shutdown path stays a `select` on `ctx.Done()` rather
than a rollback protocol.
