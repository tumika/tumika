package sqlite

import (
	"database/sql"
	"errors"

	sqlitedriver "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/tumika/tumika/source/internal/domain"
)

// mapError translates a driver error into a domain sentinel, so that the layers
// above never have to know what database is underneath. A service checks
// errors.Is(err, domain.ErrConflict); it does not parse SQLite result codes.
func mapError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ErrNotFound
	}

	var serr *sqlitedriver.Error
	if errors.As(err, &serr) {
		switch serr.Code() {
		// A unique or primary-key violation. The one that matters is the
		// partial index permitting a single live credential per provider and
		// kind: a second insert is a conflict the caller must resolve by
		// retiring the first, not a failure to paper over.
		case sqlite3.SQLITE_CONSTRAINT_UNIQUE, sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY:
			return domain.ErrConflict
		}
	}
	return err
}
