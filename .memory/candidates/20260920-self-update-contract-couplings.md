---
about: the self-update contract is coupled to the component semver on the version line, the ConfirmBoot-before-Migrate boot order, the bearer-token policy and the installer's signed-BOM discovery; what changing it breaks
saw:
  - source/daemon/internal/service/update.go
  - source/daemon/internal/daemon/daemon.go
  - source/daemon/internal/repository/sqlite/migrate.go
  - source/daemon/internal/domain/state.go
  - source/daemon/internal/api/middleware.go
  - scripts/verify-release-assets.sh
  - scripts/install-daemon.sh
  - agentic/references/http-api.md
---
Couplings that hold as of 2026-09-20:
- Field 2 of the first line of `tumika version` is the component semver, and nothing else may go there: `parseVersion` in update.go reads it positionally from the staged binary, and verify-release-assets.sh greps `^tumika <component version from release.yaml> ` (with `-snapshot` on a snapshot build). The release label lives inside the parentheses. The schema check reads `version --json` (`buildinfo.Info.SchemaVersion`) instead, decoded by `parseSchemaVersion`.
- `supersedes` in update.go is the single update rule, called by both `Check` and `Apply`. An edge daemon may move to a lower component version, so a second copy of the comparison in either method turns an offered update into a refused one.
- daemon.go runs `ConfirmBoot` BEFORE `sqlite.Migrate`. `ErrSchemaTooNew` (migrate.go) is therefore a failed boot that has ALREADY been counted: a schema-refusing binary reaches `MaxBootAttempts` (3, domain/state.go) and rolls back. That is why the pre-flight refuses a staged binary whose embedded schema is lower than the database's before the pending row is written, while the old binary is still in charge.
- The pre-flight execs `<bin> version` and `<bin> version --json` with a minimal environment and never opens the database, so the schema comparison has to use the number the staged binary reports, not a database read by it.
- Auth: http-api.md "no exemptions, including /v1/health"; middleware.go says the same. An unauthenticated version route contradicts documented policy, and `/v1/version` (which carries the release, channel and schema version) stays behind the token.
- scripts/install-daemon.sh discovers a release the same way the daemon does: it fetches a channel head (or a pinned label's document) from get.tumika.org, verifies the detached signature against a release public key embedded in the script, and only then reads the asset URL and SHA-256 out of it. The embedded key is the first entry of releaseKeyPEMs in platform/release/keys.go and TestInstallerKeyMatchesReleaseKey fails when the two drift, so a first install and a self-update sit on one trust chain rather than two.
