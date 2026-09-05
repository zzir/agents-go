package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/dialect/sqlitedialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/uptrace/bun/driver/sqliteshim"
)

// instanceLockKey identifies the single-instance advisory lock, database-wide
// on PostgreSQL. An arbitrary fixed constant so every agents-server process
// contends on the same lock — spelling "agntsgo" in ASCII.
const instanceLockKey int64 = 0x61_67_6e_74_73_67_6f

// ErrInstanceLocked is returned when another agents-server already holds the
// database's single-instance lock.
var ErrInstanceLocked = errors.New("another agents-server holds this database; run one instance per database")

// OpenDB opens the store database from the --db flag's value: a postgres:// or
// postgresql:// URL opens PostgreSQL, anything else is a SQLite file path.
func OpenDB(arg string) (*bun.DB, error) {
	if strings.HasPrefix(arg, "postgres://") || strings.HasPrefix(arg, "postgresql://") {
		return NewPostgresDB(arg), nil
	}
	return NewSQLiteDB(fmt.Sprintf("file:%s?cache=shared", arg))
}

// NewSQLiteDB opens the SQLite database at dsn with a single-connection pool.
// PRAGMAs are executed as statements, never DSN parameters: modernc and mattn
// disagree on DSN pragma syntax and silently drop what they do not recognize.
func NewSQLiteDB(dsn string) (*bun.DB, error) {
	sqldb, err := sql.Open(sqliteshim.DriverName(), dsn)
	if err != nil {
		return nil, fmt.Errorf("opening sqlite: %w", err)
	}
	sqldb.SetMaxOpenConns(1)
	// journal_mode answers with the mode now in force, so the WAL claim is
	// verified; in-memory databases report "memory".
	var mode string
	if err := sqldb.QueryRow("PRAGMA journal_mode=WAL").Scan(&mode); err != nil {
		return nil, fmt.Errorf("enabling WAL: %w", err)
	}
	if !strings.EqualFold(mode, "wal") && !strings.EqualFold(mode, "memory") {
		return nil, fmt.Errorf("sqlite journal mode is %q, not WAL", mode)
	}
	if _, err := sqldb.Exec("PRAGMA busy_timeout=5000"); err != nil {
		return nil, fmt.Errorf("setting busy_timeout: %w", err)
	}
	db := bun.NewDB(sqldb, sqlitedialect.New())
	return db, nil
}

// AcquireInstanceLock admits one agents-server per PostgreSQL database, so a
// second instance cannot run the startup orphan sweep against a live
// instance's tasks (scope.md, Roadmap). It holds a dedicated connection for
// the process's lifetime — a PostgreSQL session advisory lock lives with its
// session — and release() frees it. On SQLite (one file, one process by
// assumption) it is a no-op. Returns ErrInstanceLocked when another instance
// already holds the lock.
func AcquireInstanceLock(ctx context.Context, db *bun.DB) (release func(), err error) {
	if db.Dialect().Name() != dialect.PG {
		return func() {}, nil
	}
	conn, err := db.DB.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("instance lock: acquiring a connection: %w", err)
	}
	var got bool
	if err := conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", instanceLockKey).Scan(&got); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("instance lock: %w", err)
	}
	if !got {
		_ = conn.Close()
		return nil, ErrInstanceLocked
	}
	return func() {
		// Best-effort unlock; closing the session releases it regardless.
		_, _ = conn.ExecContext(context.Background(), "SELECT pg_advisory_unlock($1)", instanceLockKey)
		_ = conn.Close()
	}, nil
}

// NewPostgresDB opens the PostgreSQL database at dsn. A bad DSN or
// unreachable server surfaces on the first query (the instance lock at startup).
// The pool is capped so one burst cannot exhaust the server's max_connections.
func NewPostgresDB(dsn string) *bun.DB {
	sqldb := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn)))
	sqldb.SetMaxOpenConns(16)
	sqldb.SetMaxIdleConns(8)
	sqldb.SetConnMaxLifetime(time.Hour)
	return bun.NewDB(sqldb, pgdialect.New())
}
