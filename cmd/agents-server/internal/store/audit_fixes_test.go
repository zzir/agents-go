package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/zzir/agents-go/agents"
)

// a row whose entry JSON can't be deserialized must survive: the delete only
// commits after a successful decode, so a decode failure rolls back.
func TestPopEntryRollsBackOnUndecodableRow(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	sid := NewID()
	s := NewEntryStore(db, sid)

	seed(t, s, userEntry(t, "keep me"))
	// A newer row with non-empty but undecodable entry JSON.
	bad := entryRow{
		SessionID: sid, RunID: "r", EntryID: sid + "-e2",
		Kind: string(agents.EntryKindItem), Entry: `{"kind":`, CreatedAt: time.Now().UTC(),
	}
	if _, err := db.NewInsert().Model(&bad).Exec(ctx); err != nil {
		t.Fatalf("insert bad: %v", err)
	}

	if _, err := s.PopEntry(ctx); err == nil {
		t.Fatal("expected an error popping an undecodable row")
	}
	// Neither row was deleted — no silent data loss.
	var remaining []entryRow
	if err := db.NewSelect().Model(&remaining).Where("session_id = ?", sid).Scan(ctx); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(remaining) != 2 {
		t.Fatalf("rows = %d, want 2 (nothing lost on decode failure)", len(remaining))
	}
}

// deleting a task's hidden child session directly must remove the owning
// task row too, so no orphan tasks row is left pointing at a deleted session.
func TestSessionDeleteRemovesOwningTask(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	sessions := NewSessionStore(db)
	tasks := NewTaskStore(db)

	parent := &Session{ID: NewID(), Name: "parent"}
	child := &Session{ID: NewID(), Name: "child"}
	for _, s := range []*Session{parent, child} {
		if err := sessions.Create(ctx, s); err != nil {
			t.Fatalf("create session: %v", err)
		}
	}
	task := &Task{ID: NewID(), RunID: NewID(), ParentSessionID: parent.ID, ChildSessionID: child.ID, Status: "working"}
	if err := tasks.Create(ctx, task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	// Delete the hidden child session directly (e.g. via the REST endpoint).
	if err := sessions.Delete(ctx, child.ID); err != nil {
		t.Fatalf("delete child: %v", err)
	}
	if _, err := tasks.Get(ctx, task.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("owning task should be gone, got %v", err)
	}
}

// regression guard: deleting the parent still cascades to the task row and
// its hidden child session.
func TestSessionDeleteParentCascadesTask(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	sessions := NewSessionStore(db)
	tasks := NewTaskStore(db)

	parent := &Session{ID: NewID(), Name: "parent"}
	child := &Session{ID: NewID(), Name: "child"}
	for _, s := range []*Session{parent, child} {
		if err := sessions.Create(ctx, s); err != nil {
			t.Fatalf("create session: %v", err)
		}
	}
	task := &Task{ID: NewID(), RunID: NewID(), ParentSessionID: parent.ID, ChildSessionID: child.ID, Status: "working"}
	if err := tasks.Create(ctx, task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	if err := sessions.Delete(ctx, parent.ID); err != nil {
		t.Fatalf("delete parent: %v", err)
	}
	if _, err := tasks.Get(ctx, task.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("task should cascade with parent, got %v", err)
	}
	if _, err := sessions.Get(ctx, child.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("hidden child session should cascade with parent, got %v", err)
	}
}

// a missing agent config still reports ErrNotFound, and CrudStore deletes of
// other entities go through the plain delete path.
func TestAgentConfigDeleteNotFoundAndOtherEntities(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	if err := NewAgentConfigStore(db).Delete(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleting missing agent: want ErrNotFound, got %v", err)
	}
	// A Memory (also CrudStore-backed) deletes through the plain path.
	memories := NewMemoryStore(db)
	m := &Memory{Key: "k", Content: "c"}
	if err := memories.Create(ctx, m); err != nil {
		t.Fatalf("create memory: %v", err)
	}
	if err := memories.Delete(ctx, m.ID); err != nil {
		t.Fatalf("delete memory: %v", err)
	}
	if err := memories.Delete(ctx, m.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("re-delete memory: want ErrNotFound, got %v", err)
	}
}

// persistCompaction must not insert an orphan checkpoint when the entries it
// planned to fold were deleted out from under it.
func TestPersistCompactionSkipsWhenEntriesGone(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	sessionID := NewID()
	sa := NewEntryStore(db, sessionID)
	insertItemRows(t, sa, []string{userItemJSON, assistantItemJSON})
	rows := loadRows(t, db, sessionID)
	ids := []int64{rows[0].ID, rows[1].ID}

	summary, err := agents.NewCompactionEntry(agents.CompactionPayload{Summary: "sum"}, nil)
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	ca := NewCompactionAdapter(sa, &summaryFakeModel{}, 1, 1, "", CompactionNotifier{})

	// Simulate a concurrent session delete: the target entries are gone.
	if _, err := db.NewDelete().Model((*entryRow)(nil)).Where("session_id = ?", sessionID).Exec(ctx); err != nil {
		t.Fatalf("delete entries: %v", err)
	}

	applied, err := ca.persistCompaction(ctx, ids, summary)
	if err != nil {
		t.Fatalf("persistCompaction: %v", err)
	}
	if applied {
		t.Fatal("compaction must not apply when target rows are gone")
	}
	if n := len(loadRows(t, db, sessionID)); n != 0 {
		t.Fatalf("orphan checkpoint inserted into a vanished session: %d rows", n)
	}

	// Positive control: with the rows present it applies and appends the checkpoint.
	insertItemRows(t, sa, []string{userItemJSON, assistantItemJSON})
	got := loadRows(t, db, sessionID)
	ids2 := []int64{got[0].ID, got[1].ID}
	summary2, err := agents.NewCompactionEntry(agents.CompactionPayload{Summary: "sum2"}, nil)
	if err != nil {
		t.Fatalf("checkpoint 2: %v", err)
	}
	applied, err = ca.persistCompaction(ctx, ids2, summary2)
	if err != nil {
		t.Fatalf("persistCompaction (positive): %v", err)
	}
	if !applied {
		t.Fatal("compaction should apply when target rows exist")
	}
	if after := loadRows(t, db, sessionID); len(after) != 3 {
		t.Fatalf("want 3 rows (2 compacted + checkpoint), got %d", len(after))
	}
}

// ForkSession copies the source snapshot atomically, dedupes run ids, and
// leaves the source untouched.
func TestForkEntriesCopiesSnapshot(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	sessions := NewSessionStore(db)
	src := &Session{ID: NewID(), Name: "src"}
	if err := sessions.Create(ctx, src); err != nil {
		t.Fatalf("create src: %v", err)
	}
	s := NewEntryStore(db, src.ID)
	s.SetRunID("runA")
	seed(t, s, userEntry(t, "1"), userEntry(t, "2"))
	s.SetRunID("runB")
	seed(t, s, userEntry(t, "3"))

	dst := &Session{ID: NewID(), Name: "dst"}
	runIDs, err := s.ForkSession(ctx, dst, src.ID, 0, false)
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	if len(runIDs) != 2 {
		t.Fatalf("run ids = %v, want 2 deduped", runIDs)
	}
	copied, err := s.GetEntries(ctx, dst.ID, 0, 0)
	if err != nil {
		t.Fatalf("get dst: %v", err)
	}
	if len(copied) != 3 {
		t.Fatalf("dst copied %d rows, want 3", len(copied))
	}
	// Ids are rewritten into the destination's namespace, and parent links with
	// them — a fork pointing back at another session's entries is not a tree.
	for i, e := range copied {
		if e.EntryID != fmt.Sprintf("%s-e%d", dst.ID, i+1) {
			t.Fatalf("entry %d kept a foreign id: %q", i, e.EntryID)
		}
		if i > 0 && e.ParentID != copied[i-1].EntryID {
			t.Fatalf("entry %d parent = %q, want %q", i, e.ParentID, copied[i-1].EntryID)
		}
	}
	if orig, err := s.GetEntries(ctx, src.ID, 0, 0); err != nil || len(orig) != 3 {
		t.Fatalf("src changed by fork: %d rows (%v)", len(orig), err)
	}
}

// the upToID boundary is honored (inclusive vs exclusive).
func TestForkEntriesUpToBoundary(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	sessions := NewSessionStore(db)
	src := &Session{ID: NewID(), Name: "src"}
	if err := sessions.Create(ctx, src); err != nil {
		t.Fatalf("create src: %v", err)
	}
	s := NewEntryStore(db, src.ID)
	s.SetRunID("r")
	seed(t, s, userEntry(t, "1"), userEntry(t, "2"), userEntry(t, "3"))

	all, err := s.GetEntries(ctx, src.ID, 0, 0)
	if err != nil {
		t.Fatalf("get src: %v", err)
	}
	cut := all[1].ID

	inc := &Session{ID: NewID(), Name: "inc"}
	if _, err := s.ForkSession(ctx, inc, src.ID, cut, false); err != nil {
		t.Fatalf("inclusive fork: %v", err)
	}
	got, _ := s.GetEntries(ctx, inc.ID, 0, 0)
	if len(got) != 2 {
		t.Fatalf("inclusive up-to copied %d, want 2", len(got))
	}

	exc := &Session{ID: NewID(), Name: "exc"}
	if _, err := s.ForkSession(ctx, exc, src.ID, cut, true); err != nil {
		t.Fatalf("exclusive fork: %v", err)
	}
	got, _ = s.GetEntries(ctx, exc.ID, 0, 0)
	if len(got) != 1 {
		t.Fatalf("exclusive up-to copied %d, want 1", len(got))
	}
}
