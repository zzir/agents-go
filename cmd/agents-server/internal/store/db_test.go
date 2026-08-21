package store

import (
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
