package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/dialect/sqlitedialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/uptrace/bun/driver/sqliteshim"
)

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

// NewPostgresDB opens the PostgreSQL database at dsn. A bad DSN or
// unreachable server surfaces on the first query (CreateSchema at startup).
// The pool is capped so one burst cannot exhaust the server's max_connections.
func NewPostgresDB(dsn string) *bun.DB {
	sqldb := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn)))
	sqldb.SetMaxOpenConns(16)
	sqldb.SetMaxIdleConns(8)
	sqldb.SetConnMaxLifetime(time.Hour)
	return bun.NewDB(sqldb, pgdialect.New())
}
