package store

import (
	"context"
	"strings"
	"testing"
)

// A database file created by an older build must fail at startup with one
// clear message, not per-request as "no such column" — CREATE TABLE IF NOT
// EXISTS skips a table that exists in an older shape.
func TestCreateSchemaFailsOnStaleTable(t *testing.T) {
	db, err := NewSQLiteDB("file:" + NewID() + "?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()

	// A sandboxes table from before the revision counter existed.
	if _, err := db.ExecContext(ctx,
		`CREATE TABLE sandboxes (id TEXT PRIMARY KEY, name TEXT, type TEXT, config TEXT, created_at TIMESTAMP, updated_at TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}

	err = CreateSchema(ctx, db)
	if err == nil {
		t.Fatal("CreateSchema accepted a stale table; want a schema-out-of-date error")
	}
	if !strings.Contains(err.Error(), "out of date") || !strings.Contains(err.Error(), "Sandbox") {
		t.Fatalf("err = %v, want an out-of-date message naming the model", err)
	}
}

// The probe must pass on a database this build created — including one it
// re-opens (the everyday restart path).
func TestCreateSchemaIdempotentOnCurrentSchema(t *testing.T) {
	db, err := NewSQLiteDB("file:" + NewID() + "?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	if err := CreateSchema(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := CreateSchema(ctx, db); err != nil {
		t.Fatalf("second CreateSchema on the same database: %v", err)
	}
}
