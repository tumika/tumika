package sqlite

import (
	"context"
	"fmt"

	"github.com/pressly/goose/v3"

	"github.com/tumika/tumika/source/internal/domain"
	"github.com/tumika/tumika/source/internal/repository/migrations"
)

// Migrate brings the schema up to what this binary embeds.
//
// It refuses to run when the database is NEWER than this binary understands.
// That is the case a self-updating daemon actually meets: an update is applied,
// migrations run, the new binary fails to boot, and the rollback puts the
// previous binary in front of a schema it has never seen (ADR-0003). Refusing
// loudly is recoverable — the operator restores a backup or reinstalls the newer
// build. Running anyway is not: the old binary would write rows the new schema's
// constraints were meant to prevent.
func Migrate(ctx context.Context, s *Store) error {
	embedded, err := migrations.MaxVersion()
	if err != nil {
		return err
	}

	goose.SetBaseFS(migrations.FS)
	// goose's own output goes to stdout by default, which would bypass the
	// redacting slog handler and interleave with the daemon's log stream.
	goose.SetLogger(goose.NopLogger())

	if err := goose.SetDialect("sqlite3"); err != nil {
		return fmt.Errorf("set goose dialect: %w", err)
	}

	current, err := goose.GetDBVersionContext(ctx, s.rw)
	if err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}

	if current > embedded {
		return fmt.Errorf("%w: database is at version %d, this binary embeds up to %d",
			domain.ErrSchemaTooNew, current, embedded)
	}

	if err := goose.UpContext(ctx, s.rw, "."); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}

// SchemaVersion reports the migration version the database is currently at.
// Surfaced in /v1/health, and the input to the guard above.
func SchemaVersion(ctx context.Context, s *Store) (int64, error) {
	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("sqlite3"); err != nil {
		return 0, fmt.Errorf("set goose dialect: %w", err)
	}
	v, err := goose.GetDBVersionContext(ctx, s.rw)
	if err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	return v, nil
}
