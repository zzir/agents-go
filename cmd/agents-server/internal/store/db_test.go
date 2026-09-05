package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// A file-backed database must actually run in WAL mode with a busy timeout —
// the DSN-parameter spelling used to be driver-specific and silently ignored,
// leaving the default rollback journal while the docs claimed WAL.
func TestSQLiteFileDBRunsInWALMode(t *testing.T) {
	db, err := OpenDB(filepath.Join(t.TempDir(), "wal.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()

	var mode string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("reading journal_mode: %v", err)
	}
	if !strings.EqualFold(mode, "wal") {
		t.Fatalf("journal_mode = %q, want wal", mode)
	}
	var timeout int
	if err := db.QueryRow("PRAGMA busy_timeout").Scan(&timeout); err != nil {
		t.Fatalf("reading busy_timeout: %v", err)
	}
	if timeout != 5000 {
		t.Fatalf("busy_timeout = %d, want 5000", timeout)
	}
}

// On SQLite the instance lock is a no-op: one file, one process by assumption,
// so acquiring twice both succeed and release is harmless. Built directly on
// SQLite — newTestDB would be PostgreSQL when AGENTS_PG_TEST_DSN is set.
func TestInstanceLockNoOpOnSQLite(t *testing.T) {
	ctx := context.Background()
	db, err := NewSQLiteDB("file:" + NewID() + "?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	rel1, err := AcquireInstanceLock(ctx, db)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	rel2, err := AcquireInstanceLock(ctx, db)
	if err != nil {
		t.Fatalf("second acquire: %v", err)
	}
	rel1()
	rel2()
}
