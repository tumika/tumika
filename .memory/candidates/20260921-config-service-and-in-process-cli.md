---
about: ConfigService contract (closed keys, secret hidden as unknown, reset = delete row) and that the CLI's in-process exception is now a named, bounded rule (ADR-0013) rather than an unresolved conflict with ADR-0004
saw:
  - source/daemon/internal/service/config.go
  - source/daemon/internal/cli/daemonhelper.go
  - source/daemon/internal/cli/config.go
  - docs/adr/0004-layered-architecture.md
  - docs/adr/0013-cli-config-command-is-in-process.md
  - source/daemon/internal/api/config.go
---

- settingDefinitions is closed; Set is all-or-nothing in one tx; validate is strict per kind (bool/string/duration positive, canonicalised via d.String()/address host:port/enum).
- Secret key server.api_token_sha256 is reported as ErrUnknownSetting (not forbidden) by Get/List/Set/Reset (config.go public()).
- Reset = repo.Delete (config_repo.go:61); IsSet false afterwards; writing the default sets IsSet true. Reset of an unset key is not an error at the service (sqlc DELETE, no rows check).
- Sentinels: ErrUnknownSetting, ErrInvalidSetting; API maps 404 unknown_setting / 400 invalid_setting (api/api.go:138).
- ADR-0004's decision bullet and status line are narrowed by ADR-0013: in-process CLI access is allowed only for a command that must work without a serving daemon. `token`, `update`, and now `config set`/`reset` qualify; `config list`/`get` also run in-process (docs/adr/0013-cli-config-command-is-in-process.md) so the four subcommands share one access path rather than splitting on daemon availability.
- `source/daemon/internal/cli/config.go` declares a local `configService` interface (`Definitions`/`Get`/`List`/`Set`/`Reset`) narrower than `service.ConfigService`, deliberately omitting `ReadSecret`/`WriteSecret` so no CLI code path can compile against a secret setting.
- withDaemon runs daemon.New -> sqlite.Open + Migrate (daemon.go:136-190) with SkipUpdateBoot; store WAL + busy_timeout 5000 (repository/sqlite/store.go:44).
