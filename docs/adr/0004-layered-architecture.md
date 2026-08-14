---
status: accepted
date: 2026-08-14
---

# The daemon is layered API → Service → Repository, with single-owner repositories, enforced by depguard

This branch builds foundations for a system whose interesting parts do not exist yet: a
workflow engine, connectors, approvals, a UI. Those will arrive under delivery pressure, into a
codebase whose shape is already set. The purpose of this decision is that stage 2 *extends* the
structure rather than refactoring it.

We decided on a strict layering, an explicit ownership rule for repositories, and — critically
— a mechanical enforcement mechanism, because a documented architecture that only a reviewer
enforces is an architecture with a half-life.

## Decisions

- **Four layers plus two supporting packages:**

  ```
  CLI ──┐
        ├──► API ──► SERVICE ──► REPOSITORY ──► SQLite
  RUNNER┘              │
                       └──────► PLATFORM (provider · secrets · servicemgr · release)
  ```

  **API** is transport only — decode, call exactly one service method, encode. **SERVICE** owns
  all business logic and every transaction boundary, and is the only layer that may call a
  repository. **REPOSITORY** is data access returning domain types, never `*sql.Rows`.
  **RUNNER** is a supervised long-lived process depending on services only. **PLATFORM** holds
  infrastructure abstractions, injected as interfaces. **DOMAIN** holds shared types and
  imports nothing of ours.

- **A repository has exactly one owning service.** No repository is shared. When a service
  needs data owned by another, it calls that *service*. The worked case: a successful login
  makes `LoginService` call `ProviderService.StoreCredential(...)` rather than touching
  `CredentialRepository`, which is where the AAD binding and the verify-before-active rule
  live.

- **The CLI is an HTTP client of the daemon**, not a second entry point into the services. One
  API surface, one set of business rules, and the CLI works identically against a local or a
  remote daemon.

- **`depguard` in `.golangci.yml` is the enforcement point.** `api` may not import
  `repository`; `repository` may not import `service` or `api`; `runner` may not import
  `repository`; `platform` may not import `service`, `repository` or `api`; `domain` imports
  none of ours. A violation is a red build.

- **The rules live in `.agents/rules/`**, one intent-named file each, in the agentic toolkit's
  existing convention — so `wrap-session` maintains them and every agent loads them
  automatically. Each layering rule names `.golangci.yml` as its enforcement point under
  `## Applies to`, and the two change in the same commit.

- **All source lives under `/source`**, with one `go.mod` at the repo root. `internal/`
  visibility is scoped to its own parent, so `source/internal/...` is importable throughout
  `source/`, which is all of our code.

## Considered alternatives

- **A flat package layout with discipline.** Rejected: it is what the codebase would become
  under pressure anyway, and there is nothing to enforce. The failure is not dramatic — it is
  one query in a handler, then a second, and eighteen months later the business rules are
  spread over three layers.
- **Documenting the layering in `AGENTS.md` only, without `depguard`.** Rejected: this was the
  decisive point. Documentation makes a violation a *review argument*, which is resolved by
  whoever has more time that day. `depguard` makes it a build failure, which is not an argument
  at all.
- **Sharing repositories between services where convenient.** Rejected: the shared repository
  is how a data model with invariants degrades into a table with two writers, and the second
  writer silently skips the invariants without anything in the code saying so.
- **Letting the CLI call services in-process.** Rejected: two entry points means two places for
  a rule to live, and it forecloses the remote-daemon case that the CLI-over-HTTP design gets
  for free.

## Consequences

- **More indirection than the current feature set needs.** `/v1/config` is a handler, a service
  and a repository for what is fundamentally a key-value read. That is the price, paid
  deliberately and up front, and step 4 exists precisely to walk the simplest possible feature
  through every layer before anything complicated arrives.
- **Business logic becomes testable without a database or HTTP** — services are tested against
  in-memory fakes of the repository interfaces. This is the concrete payoff, and it is what
  makes the indirection worth paying for.
- Cross-service calls form a directed graph (`LoginService → ProviderService`,
  `HealthService → ProviderService`). A change that needs a reverse edge is a signal that two
  services are really one, or that a third is missing — not a reason to inject the repository.
- Changing a layering rule requires editing `.golangci.yml`, the corresponding rule file, and
  the code, together. That friction is intentional.
- Stage 2 slots in without structural change: `WorkflowRunner` implements `Runner`, and
  `Inferencer` is added to the provider drivers as another optional capability interface.
