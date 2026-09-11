package store

import (
	"context"
	"strings"
	"testing"

	"github.com/uptrace/bun/dialect"
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

// CREATE INDEX IF NOT EXISTS keeps an index of an older shape; the probe must
// name it at startup instead of letting duplicate seqs in at run time.
func TestCreateSchemaFailsOnStaleUniqueIndex(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	for _, stmt := range []string{
		`DROP INDEX idx_entries_session_seq`,
		`CREATE UNIQUE INDEX idx_entries_session_seq ON entries (session_id, seq)`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	err := CreateSchema(ctx, db)
	if err == nil {
		t.Fatal("CreateSchema accepted an index without the generation column; want a schema-out-of-date error")
	}
	if !strings.Contains(err.Error(), "out of date") || !strings.Contains(err.Error(), "idx_entries_session_seq") {
		t.Fatalf("err = %v, want an out-of-date message naming the index", err)
	}
}

// Every shape the probe reads must pass on a database this build created,
// on either dialect: the partial and expression indexes included.
func TestVerifyIndexesAcceptsTheCurrentShapes(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	indexes := schemaIndexes(db.Dialect().Name() == dialect.PG)
	defs, err := indexDefinitions(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	for _, ix := range indexes {
		if _, ok := defs[ix.name]; !ok {
			t.Errorf("index %s was not created", ix.name)
		}
		if ix.unique {
			if err := ix.matches(defs[ix.name]); err != nil {
				t.Errorf("index %s: %v (def %q)", ix.name, err, defs[ix.name])
			}
		}
	}
	// The probe reads shape, not presence: a definition missing a column,
	// a non-unique one and the wrong partial literal are each refused.
	seq := indexes[0]
	for _, def := range []string{
		`CREATE UNIQUE INDEX idx_entries_session_seq ON entries (session_id, seq)`,
		`CREATE INDEX idx_entries_session_seq ON entries (session_id, gen, seq)`,
	} {
		if seq.matches(def) == nil {
			t.Errorf("accepted %q", def)
		}
	}
	global := schemaIndex{unique: true, columns: []string{"name"}, where: "scope = 'global'"}
	if global.matches(`CREATE UNIQUE INDEX x ON agent_configs (name) WHERE scope = 'private'`) == nil {
		t.Error("accepted the wrong partial predicate")
	}
}
