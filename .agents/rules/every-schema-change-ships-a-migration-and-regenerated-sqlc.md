# A schema change ships its goose migration and its regenerated sqlc output in the same commit

The database schema has exactly one source of truth: the goose migrations in
`source/internal/repository/migrations/`, embedded with `//go:embed` and applied by the daemon
at startup. There is no hand-maintained `schema.sql` that drifts, and no `CREATE TABLE`
executed anywhere else.

Three artifacts must move together, in **one commit**:

1. a new **goose migration** — never an edit to an already-released one;
2. any affected `queries/*.sql`;
3. the **regenerated `sqlc` output** under `repository/sqlite/` (`sqlc generate`).

CI runs `sqlc diff`, which fails when the checked-in generated code does not match what the
queries produce. That job is what makes this rule mechanical rather than aspirational — a
commit that changes a query without regenerating is a red build, not a surprise at runtime.

**Migrations are append-only once released.** Editing a released migration means every existing
install has a schema goose believes is current but which no longer matches the file. Every
migration has a working `-- +goose Down`, and up/down is exercised against a temp database in
the test suite.

**SQLite's ALTER TABLE is limited**, so a column change is usually the 12-step
create-new/copy/drop/rename dance, inside the migration, inside a transaction. Write it out;
do not work around a limitation by widening a column's meaning instead of changing its type.

**The schema-version guard is part of this contract.** If the database's goose version is
*newer* than the running binary knows about, the daemon **refuses to start** rather than
operating against a schema it does not understand. That is the safety net for a rollback after
an update: an older binary meeting a newer database stops loudly instead of corrupting data.
Before running migrations during an update, `VACUUM INTO` a backup and keep the last three.

**Keep `.sql` files ASCII-only.** sqlc's SQLite parser computes statement offsets
inconsistently between bytes and runes, so a single multi-byte character *in a comment*
silently shifts every statement after it in the generated file. One em dash produced:

```go
const setProviderEnabled = `-- name: SetProviderEnabled :exec
t;
UPDATE providers ... WHERE id =
```

That compiles, reads plausibly, survives review, and fails at runtime with
`no such column`. `sqlc diff` does not catch it either — the generated file matches what
sqlc produces, because sqlc produces the corruption. `TestQueryFilesAreASCII` in
`repository/sqlite` is the guard, alongside a check that no generated query ends in a
dangling operator.

## Applies to

| | |
|---|---|
| `source/internal/repository/migrations/*.sql` | goose migrations, `//go:embed`ed; append-only once released |
| `source/internal/repository/queries/*.sql` | sqlc input |
| `source/internal/repository/sqlite/**` | **generated** — never hand-edit; regenerate |
| `sqlc.yaml` | engine `sqlite`; changing it means regenerating everything |
| `repository/sqlite/generated_test.go` | guards the ASCII rule above, and that no generated query is truncated |
| `.github/workflows/ci.yml` (`sqlc` job) | enforcement point: `sqlc diff` must be clean |
| `UpdateService.ConfirmBoot` / daemon startup | the schema-version guard and the pre-migration backup |

## Example

**WRONG** — the schema is edited where it is convenient:

```go
// repository/sqlite/db.go
func Open(path string) (*sql.DB, error) {
    db, err := sql.Open("sqlite", dsn(path))
    if err != nil { return nil, err }
    // "just one column, a migration is overkill"
    _, err = db.Exec(`ALTER TABLE provider_credentials ADD COLUMN account_email TEXT`)
    return db, err
}
```

It runs on every start, it is invisible to goose, and the next real migration is written
against a schema that only exists on machines that ran this build.

**RIGHT** — `0002_credential_account_email.sql`:

```sql
-- +goose Up
ALTER TABLE provider_credentials ADD COLUMN account_email TEXT;

-- +goose Down
ALTER TABLE provider_credentials DROP COLUMN account_email;
```

then update the query and regenerate in the same commit:

```sh
sqlc generate && sqlc diff   # diff must be clean; both outputs are committed together
```

## Why

tumika self-updates and can roll back. A binary and a database therefore meet in combinations
nobody tested interactively: new binary on old schema (handled — migrations run forward), and
old binary on new schema (handled — the guard refuses to start). Both depend on the goose
version being an honest record of what has been applied. An out-of-band `ALTER TABLE` breaks
that record, and it breaks it on *users' machines*, where the only recovery is the backup.

The same reasoning is why `sqlc diff` gates CI rather than being a habit: generated code that
disagrees with the schema fails at the exact query, at runtime, on the one code path nobody
exercised before shipping.
