# A repository has exactly one owning service; every other caller goes through that service

Each repository interface is constructed and held by **one** service. No repository is injected
into two services, and no service reaches into a repository it does not own.

The ownership map is closed. Adding a repository means adding a row.

| Service | Owns repositories |
|---|---|
| `ConfigService` | `ConfigRepository` |
| `ProviderService` | `ProviderRepository`, `CredentialRepository` |
| `LoginService` | `LoginSessionRepository` |
| `UpdateService` | `UpdateStateRepository` |

When a service needs data owned by another, it calls **that service**, never its repository.
The owning service's method is where the invariants live: sealing with the right AAD, enforcing
the partial unique index's intent, marking status, emitting the audit trail. A second caller
going straight to the repository does not get any of that, and — worse — nothing about the code
says so. It compiles, the tests pass, and the invariant is simply absent on that path.

This is a stricter rule than "don't share state", and deliberately so. A shared repository is
how a data model with rules degrades into a table with two writers.

## Applies to

| | |
|---|---|
| `source/internal/service/**` | constructors take only their own repositories |
| `source/internal/daemon/**` | the composition root; the only place repositories are wired, and therefore the only place this rule can be violated by construction |
| `source/internal/runner/**` | never receives a repository at all — see `runners-depend-on-services-never-repositories.md` |
| `.golangci.yml` (`depguard`) | enforces the coarse half: `runner` and `api` may not import `repository` at all. Cross-service repository sharing is **not** mechanically detectable — it is caught in `daemon`'s wiring at review time |

## Example

The interesting case is real, not hypothetical. When an interactive login succeeds,
`LoginService` holds a freshly captured OAuth token and must persist it — but credentials are
owned by `ProviderService`.

**WRONG** — `LoginService` is handed `CredentialRepository`:

```go
type LoginService struct {
    sessions repository.LoginSessionRepository
    creds    repository.CredentialRepository  // not ours
    sealer   secrets.Sealer
}

func (s *LoginService) finish(ctx context.Context, sess domain.LoginSession, token string) error {
    sealed, err := s.sealer.Seal([]byte(token), []byte(sess.ProviderID))  // AAD is wrong:
    if err != nil { return err }                                          // it must be provider|kind
    return s.creds.Upsert(ctx, domain.SealedCredential{…})                // status never verified
}
```

Two invariants silently lost: the AAD binding that stops ciphertext being transplanted between
rows, and the rule that a stored credential is verified before it is marked active. Both live
in `ProviderService.StoreCredential`, which this path never calls.

**RIGHT** — `LoginService` depends on `ProviderService`:

```go
type LoginService struct {
    sessions  repository.LoginSessionRepository
    providers ProviderService   // the owner; not its repository
}

func (s *LoginService) finish(ctx context.Context, sess domain.LoginSession, token string) error {
    return s.providers.StoreCredential(ctx, sess.ProviderID, domain.Credential{
        Kind:   "oauth_token",
        Secret: token,
    })
}
```

`CredentialMonitorRunner` follows the same shape: it calls
`ProviderService.VerifyCredential(...)`, not `CredentialRepository.UpdateStatus(...)`.

## Why

Service-to-service calls are a directed graph, and a cycle in it deadlocks a transaction as
surely as it confuses a reader. The current edges are `LoginService → ProviderService` and
`HealthService → ProviderService`, both one-way. If a change needs the reverse edge, that is
the signal that the two services are really one, or that a third one is missing — resolve it by
moving code, not by injecting the repository and calling it a shortcut.
