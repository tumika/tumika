# Provider model

A provider is an LLM backend. Two ship in this branch:

| ID | Kind | Auth methods | Installs a binary |
|---|---|---|---|
| `claude-code` | `cli` | `manual_token` | yes (vendored `claude`) |
| `anthropic-api` | `http` | `api_key` | no |

`claude-code` will also offer `interactive_cli` once the PTY login lands. It does **not**
declare it today, and must not: the registry validates the descriptor against the interfaces
actually implemented, so declaring a method ahead of its implementation stops the daemon at
startup. The absence is discoverable by type assertion, which is what will let the login
endpoint refuse with `400 interactive_auth_unsupported` when it lands. **There is no
`POST /v1/providers/{id}/login` route yet** — that path answers 404 today, not 400.

Providers **declare their capabilities by which interfaces they implement**, and the registry
discovers them by type assertion. `Provider` and `HealthChecker` are mandatory;
`StaticAuthenticator`, `InteractiveAuthenticator` and `Installer` are optional. Clients read
`requires_interactive_auth` from the descriptor to decide whether to `PUT` a secret or drive
the login-session endpoints.

`anthropic-api` exists precisely so the abstraction is not silently Claude-CLI-shaped: it
implements neither `Installer` nor `InteractiveAuthenticator`.

**The registry validates the correspondence at construction**, so a driver whose descriptor
disagrees with the interfaces it implements stops the daemon at startup rather than producing a
client that offers a flow the daemon rejects. The compiler cannot check that, which is why the
registry does. Every driver must also pass the shared suite in
`platform/provider/providertest` — written once, run against every implementation, so a second
driver cannot quietly diverge from the first.

**Credentials are stored before they are verified, deliberately.** Sealing and insertion happen
in one transaction; verification is a network call made holding *no* transaction; the verdict
lands in a second. Verifying inside the transaction would hold SQLite's single write lock across
a network call, so a hanging provider would block every other write in the daemon. This is why
the schema has an `unverified` status at all.
