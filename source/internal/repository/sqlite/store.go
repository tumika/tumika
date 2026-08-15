package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"

	// Registers the "sqlite" driver. Pure Go, so CGO_ENABLED=0 holds and
	// cross-compiling for linux/arm64 stays free — see AGENTS.md, Conventions.
	_ "modernc.org/sqlite"
)

// Store owns the database handles and the transaction boundary. It is
// constructed once in the composition root and handed to the repositories.
type Store struct {
	// rw is the single writer. SQLite permits exactly one writer at a time, so
	// the pool is capped at one connection: contention then queues inside
	// database/sql rather than surfacing as SQLITE_BUSY from the driver.
	rw *sql.DB
	// ro serves reads. Under WAL, readers do not block the writer and the
	// writer does not block them, so this is where read concurrency comes from.
	ro *sql.DB

	path string
}

// writerPragmas is the DSN for the writing handle.
//
//   - journal_mode(WAL): readers and the writer stop blocking each other, which
//     is the whole reason for a separate read handle.
//   - busy_timeout(5000): wait for a lock rather than failing immediately.
//   - foreign_keys(ON): SQLite disables FK enforcement by default, per
//     connection. Without this the ON DELETE CASCADE in the schema is decoration.
//   - synchronous(NORMAL): the WAL-appropriate setting; FULL costs an fsync per
//     commit for a durability guarantee WAL already provides against everything
//     short of power loss.
//   - _txlock=immediate: take the write lock when the transaction begins rather
//     than on its first write. Otherwise a read-then-write transaction can fail
//     to upgrade and return SQLITE_BUSY at commit, which is the classic SQLite
//     deadlock and is invisible until it happens under load.
const writerPragmas = "_pragma=journal_mode(WAL)" +
	"&_pragma=busy_timeout(5000)" +
	"&_pragma=foreign_keys(ON)" +
	"&_pragma=synchronous(NORMAL)" +
	"&_txlock=immediate"

// readerPragmas mirrors the writer, minus the write-lock behaviour, plus
// query_only so a stray write through the read handle fails loudly at the
// database instead of silently succeeding on the wrong connection.
const readerPragmas = "_pragma=busy_timeout(5000)" +
	"&_pragma=foreign_keys(ON)" +
	"&_pragma=query_only(true)"

func dsn(path, pragmas string) string {
	return "file:" + url.PathEscape(filepath.ToSlash(path)) + "?" + pragmas
}

// Open opens the database and returns a Store. It does not migrate; call
// Migrate for that, so the caller controls when schema changes happen relative
// to a backup.
func Open(ctx context.Context, path string) (*Store, error) {
	rw, err := sql.Open("sqlite", dsn(path, writerPragmas))
	if err != nil {
		return nil, fmt.Errorf("open database %s: %w", path, err)
	}
	rw.SetMaxOpenConns(1)
	rw.SetMaxIdleConns(1)

	if err := rw.PingContext(ctx); err != nil {
		_ = rw.Close()
		return nil, fmt.Errorf("connect to database %s: %w", path, err)
	}

	ro, err := sql.Open("sqlite", dsn(path, readerPragmas))
	if err != nil {
		_ = rw.Close()
		return nil, fmt.Errorf("open database %s for reading: %w", path, err)
	}
	if err := ro.PingContext(ctx); err != nil {
		_ = rw.Close()
		_ = ro.Close()
		return nil, fmt.Errorf("connect to database %s for reading: %w", path, err)
	}

	return &Store{rw: rw, ro: ro, path: path}, nil
}

// Path is the database file, surfaced in /v1/health.
func (s *Store) Path() string { return s.path }

// Close closes both handles, returning the first error while still closing the
// second.
func (s *Store) Close() error {
	roErr := s.ro.Close()
	rwErr := s.rw.Close()
	return errors.Join(rwErr, roErr)
}

// txKey carries an open transaction on the context.
type txKey struct{}

// InTx runs fn inside a single write transaction, committing when it returns
// nil and rolling back otherwise.
//
// A nested call joins the transaction already in flight rather than opening a
// second one — SQLite has a single writer, so a second BEGIN from within the
// same logical unit of work would deadlock against the first.
func (s *Store) InTx(ctx context.Context, fn func(ctx context.Context) error) error {
	if _, ok := ctx.Value(txKey{}).(*sql.Tx); ok {
		return fn(ctx)
	}

	tx, err := s.rw.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}

	// A panic must not leave the transaction open holding the write lock —
	// with SetMaxOpenConns(1) that would wedge every subsequent write.
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
	}()

	if err := fn(context.WithValue(ctx, txKey{}, tx)); err != nil {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			return errors.Join(err, fmt.Errorf("rollback: %w", rbErr))
		}
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

// write returns the handle for a statement that modifies data: the ambient
// transaction if there is one, otherwise the writer.
func (s *Store) write(ctx context.Context) DBTX {
	if tx, ok := ctx.Value(txKey{}).(*sql.Tx); ok {
		return tx
	}
	return s.rw
}

// read returns the handle for a query.
//
// Inside a transaction it must be the transaction itself — reading through the
// separate handle would not see the transaction's own uncommitted writes, so a
// service that writes and then reads back would silently get stale data.
func (s *Store) read(ctx context.Context) DBTX {
	if tx, ok := ctx.Value(txKey{}).(*sql.Tx); ok {
		return tx
	}
	return s.ro
}

// writeQ and readQ build a sqlc Queries bound to the right handle.
func (s *Store) writeQ(ctx context.Context) *Queries { return New(s.write(ctx)) }
func (s *Store) readQ(ctx context.Context) *Queries  { return New(s.read(ctx)) }
