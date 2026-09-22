# Commands and module layout

```sh
go build ./...                    # build
go test -race ./...               # run tests (race detector on, as CI does)
golangci-lint run ./...           # lint — must be clean before a PR; enforces the layering rules
bulwark scan                      # gosec + govulncheck + semgrep, exactly as CI runs them
bulwark coverage                  # diff coverage against the cached baseline (the CI gate)
sqlc generate                     # regenerate repository/sqlite from queries/ + migrations/
sqlc diff                         # fail if the committed generated code is stale (the CI gate)
go run ./source/daemon/cmd/tumika        # run the CLI locally

# First run: the daemon refuses to serve without an API token.
go run ./source/daemon/cmd/tumika token rotate   # mint one, printed once
go run ./source/daemon/cmd/tumika serve          # run the daemon in the foreground

# Read and change daemon settings in-process, whether or not a daemon is serving
go run ./source/daemon/cmd/tumika config list              # every known setting, aligned text table
go run ./source/daemon/cmd/tumika config get update.channel
go run ./source/daemon/cmd/tumika config set update.auto_apply false
go run ./source/daemon/cmd/tumika config reset update.auto_apply   # falls back to its default

# Release build dry-run (produces dist/). Both variables are mandatory: the
# component version names every asset, and an unset one fails the build with
# `map has no entry for key` rather than guessing.
export TUMIKA_DAEMON_VERSION=$(scripts/release-component-version.sh daemon)
export TUMIKA_RELEASE=$(scripts/release-label.sh)
go run github.com/goreleaser/goreleaser/v2@latest release --config source/daemon/.goreleaser.yml --snapshot --clean
```

- Module path: `github.com/tumika/tumika/source/daemon`. Go directive: `go 1.26`.
- `sqlc` is a build-time tool, deliberately **not** a module dependency — its own
  dependency tree would otherwise enter ours. Install the version CI pins:
  `go install github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1`.
- **All Go source lives under `/source`.** The `go.mod` stays at the repo root, so package
  paths are `github.com/tumika/tumika/source/daemon/internal/...` and goreleaser's `main:` is
  `./source/daemon/cmd/tumika`. This is valid Go — `internal/` visibility is scoped to its own
  parent, and `source/` contains all of our code.
