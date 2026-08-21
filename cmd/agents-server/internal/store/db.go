package store

import (
	"database/sql"
	"fmt"
	"strings"

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
	return NewSQLiteDB(fmt.Sprintf("file:%s?cache=shared&_journal_mode=WAL", arg))
}

// NewSQLiteDB opens the SQLite database at dsn and returns a bun.DB. The pool is
// limited to a single connection because SQLite serializes writes.
func NewSQLiteDB(dsn string) (*bun.DB, error) {
	sqldb, err := sql.Open(sqliteshim.DriverName(), dsn)
	if err != nil {
		return nil, fmt.Errorf("opening sqlite: %w", err)
	}
	sqldb.SetMaxOpenConns(1)
	db := bun.NewDB(sqldb, sqlitedialect.New())
	return db, nil
}

// NewPostgresDB opens the PostgreSQL database at dsn and returns a bun.DB. A
// bad DSN or unreachable server surfaces on the first query — CreateSchema at
// startup, in practice.
func NewPostgresDB(dsn string) *bun.DB {
	sqldb := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn)))
	return bun.NewDB(sqldb, pgdialect.New())
}
