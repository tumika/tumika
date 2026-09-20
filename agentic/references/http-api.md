# HTTP API

Every route is behind a bearer token — there are no exemptions, including
`/v1/health`. The daemon stores only the token's SHA-256, so it can never
recover a lost token; one is replaced (`tumika token rotate`). On macOS the
plaintext is also handed to the login Keychain at mint time (ADR-0005). The
daemon refuses to start rather than listen unauthenticated.

Middleware, outermost first:

| Order | Middleware | Why there |
|---|---|---|
| 1 | recovery | outermost, so a panic *anywhere* below becomes a 500 rather than a dropped connection |
| 2 | logging | above the security checks, so refusals are logged — a burst of 401s is what a probe looks like from the inside |
| 3 | Host allowlist | the DNS-rebinding defence; runs before auth because it exists to turn away unauthenticated probes. Literal IPs pass: they cannot be rebound |
| 4 | Origin check | no `Origin` (curl, the CLI) passes; a browser origin must be allowed. No CORS headers are set anywhere |
| 5 | bearer token | constant-time compare against the stored hash |

Note this differs from the plan's literal ordering, which put recovery and
logging innermost. Placed there, recovery would not cover a panic in the layers
above it and logging would never see a rejected request — both of which defeat
the point of having them.

## `/v1/version`

Behind the bearer token like every other route; there is no unauthenticated
variant, because an open version route fingerprints the daemon. It answers with
one report from one service call:

- the build identity: `version` (the component version), `release`, `commit`,
  `date`, `go`, `platform`, `claude_cli`, and `schema_version` (the highest
  database migration the binary embeds);
- `channel`, the update channel the daemon follows, empty when the setting
  cannot be read;
- `update`, the self-update state, present only when self-update applies and the
  row can be read.

`tumika version --json` reports the build identity only. It has no `channel`,
because it runs offline without a daemon or a database; the channel is a
daemon setting, so ask this endpoint for it.
