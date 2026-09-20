---
about: the self-update contract is coupled to the git tag, release asset name, semver-only Newer, and a ConfirmBoot-before-Migrate boot order; what changing it breaks
saw:
  - source/daemon/internal/platform/release/release.go
  - source/daemon/internal/service/update.go
  - source/daemon/internal/daemon/daemon.go
  - source/daemon/internal/repository/sqlite/migrate.go
  - source/daemon/internal/api/middleware.go
  - scripts/verify-release-assets.sh
  - scripts/install.sh
  - agentic/references/http-api.md
---
Couplings found (pointers as of 2026-09-20):
- Tag == version == asset name: release.go:107 assetURL builds `releases/download/v<version>/<asset>`; AssetName (release.go:103) is `tumika_<version>_<os>_<arch>`; Latest (release.go:111-150) parses the /releases/latest redirect tag as semver. verify-release-assets.sh takes VERSION from goreleaser metadata.json and asserts asset name and `tumika <VERSION> ` from the binary; install.sh:37-44 also uses /releases/latest. release.yml:199 derives version from GITHUB_REF_NAME.
- Newer (release.go:296) is semver-strictly-greater; used by Check (update.go:~133) and Apply (update.go:~155). Apply also compares preflight-reported version to the requested version (update.go ~ preflight block).
- daemon.go:145 ConfirmBoot runs BEFORE sqlite.Migrate (daemon.go:160). ErrSchemaTooNew (migrate.go:43) is therefore a failed boot that has ALREADY been counted: a schema-refusing binary hits MaxBootAttempts=3 (domain/state.go:31) and rolls back; the ADR-0003 consequence text relies on this.
- Auth: http-api.md:3 "no exemptions, including /v1/health"; middleware.go:194-199 comment says the same. Unauthenticated route contradicts documented policy; Host/Origin middleware (order 3,4) still apply.
- pre-flight `<bin> version` runs with a minimal env and never opens the DB (daemon.go ~135 comment), so a schema check needs a new subcommand/flag, and needs the same isolation.
