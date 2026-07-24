package store

import (
	"context"
	"testing"
)

// TestForkSessionAtomicNoOrphan verifies ForkSession is atomic: when the session
// insert fails, the message copy in the same transaction is rolled back too, so
// no orphan session or messages are left behind (the gap the old
// create-then-copy handler left open).
func TestForkSessionAtomicNoOrphan(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	sessions := NewSessionStore(db)
	messages := NewMessageStore(db)

	src := &Session{ID: NewID(), Name: "src"}
	if err := sessions.Create(ctx, src); err != nil {
		t.Fatalf("create src: %v", err)
	}
	row := NewItemMessageRaw(src.ID, "r", "m", []byte(`{"role":"user","content":"hi"}`))
	if _, err := db.NewInsert().Model(&row).Exec(ctx); err != nil {
		t.Fatalf("seed message: %v", err)
	}

	// Reuse an existing id so the session insert inside ForkSession fails.
	taken := &Session{ID: NewID(), Name: "taken"}
	if err := sessions.Create(ctx, taken); err != nil {
		t.Fatalf("create taken: %v", err)
	}
	dst := &Session{ID: taken.ID, Name: "fork"}

	if _, err := messages.ForkSession(ctx, dst, src.ID, 0, false); err == nil {
		t.Fatal("expected ForkSession to fail on a duplicate session id")
	}
	var copied []Message
	if err := db.NewSelect().Model(&copied).Where("session_id = ?", taken.ID).Scan(ctx); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(copied) != 0 {
		t.Fatalf("failed fork left %d orphan message(s)", len(copied))
	}
}

// TestForkSessionCopiesAtomically verifies the happy path: the dst session and
// its messages are both created, and the run ids are returned.
func TestForkSessionCopiesAtomically(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	sessions := NewSessionStore(db)
	messages := NewMessageStore(db)

	src := &Session{ID: NewID(), Name: "src"}
	if err := sessions.Create(ctx, src); err != nil {
		t.Fatalf("create src: %v", err)
	}
	row := NewItemMessageRaw(src.ID, "run1", "m", []byte(`{"role":"user","content":"hi"}`))
	if _, err := db.NewInsert().Model(&row).Exec(ctx); err != nil {
		t.Fatalf("seed message: %v", err)
	}

	dst := &Session{ID: NewID(), Name: "fork"}
	runIDs, err := messages.ForkSession(ctx, dst, src.ID, 0, false)
	if err != nil {
		t.Fatalf("ForkSession: %v", err)
	}
	if got, err := sessions.Get(ctx, dst.ID); err != nil || got == nil {
		t.Fatalf("dst session missing after fork: %v", err)
	}
	var copied []Message
	if err := db.NewSelect().Model(&copied).Where("session_id = ?", dst.ID).Scan(ctx); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(copied) != 1 {
		t.Fatalf("expected 1 copied message, got %d", len(copied))
	}
	if len(runIDs) != 1 || runIDs[0] != "run1" {
		t.Fatalf("expected run ids [run1], got %v", runIDs)
	}
}
