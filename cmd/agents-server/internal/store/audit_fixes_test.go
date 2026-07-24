package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/zzir/agents-go/agents"
)

// displayOf returns the decoded display projection of the tool_call row with
// call_id "c1" (the call id used throughout these tests).
func displayOf(t *testing.T, db *bun.DB, sessionID string) map[string]any {
	t.Helper()
	var rows []Message
	if err := db.NewSelect().Model(&rows).
		Where("session_id = ?", sessionID).
		Where("role = ?", "tool_call").
		OrderExpr("id DESC").
		Scan(context.Background()); err != nil {
		t.Fatalf("scan tool_call rows: %v", err)
	}
	for _, r := range rows {
		var d map[string]any
		if len(r.Display) == 0 || json.Unmarshal(r.Display, &d) != nil {
			continue
		}
		if d["call_id"] == "c1" {
			return d
		}
	}
	t.Fatalf("no tool_call row with call_id %q", "c1")
	return nil
}

// a late/reordered patch carrying a non-terminal status must never roll a
// terminal card back.
func TestPatchToolCallDisplayTerminalNotReverted(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	ms := NewMessageStore(db)
	sid := NewID()

	row := NewItemMessageRaw(sid, "r1", "m", []byte(`{"type":"function_call","call_id":"c1","name":"spawn_task","arguments":"{}"}`))
	if _, err := db.NewInsert().Model(&row).Exec(ctx); err != nil {
		t.Fatalf("insert: %v", err)
	}

	if err := ms.PatchToolCallDisplay(ctx, sid, "c1", map[string]any{"task_id": "t1", "task_status": "completed", "task_summary": "done"}); err != nil {
		t.Fatalf("terminal patch: %v", err)
	}
	// A stale patch describing the earlier input_required state arrives late.
	if err := ms.PatchToolCallDisplay(ctx, sid, "c1", map[string]any{"task_status": "input_required"}); err != nil {
		t.Fatalf("late patch: %v", err)
	}
	d := displayOf(t, db, sid)
	if d["task_status"] != "completed" {
		t.Fatalf("terminal card reverted: task_status = %v, want completed", d["task_status"])
	}
	if d["task_summary"] != "done" {
		t.Fatalf("terminal summary lost: %v", d["task_summary"])
	}

	// A competing terminal (failed) must not override the first terminal either.
	if err := ms.PatchToolCallDisplay(ctx, sid, "c1", map[string]any{"task_status": "failed"}); err != nil {
		t.Fatalf("competing terminal patch: %v", err)
	}
	if got := displayOf(t, db, sid)["task_status"]; got != "completed" {
		t.Fatalf("first terminal not sticky: %v", got)
	}
}

// forward transitions (non-terminal → non-terminal → terminal) all apply.
func TestPatchToolCallDisplayForwardTransitions(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	ms := NewMessageStore(db)
	sid := NewID()

	row := NewItemMessageRaw(sid, "r1", "m", []byte(`{"type":"function_call","call_id":"c1","name":"spawn_task","arguments":"{}"}`))
	if _, err := db.NewInsert().Model(&row).Exec(ctx); err != nil {
		t.Fatalf("insert: %v", err)
	}
	for _, st := range []string{"working", "input_required", "completed"} {
		if err := ms.PatchToolCallDisplay(ctx, sid, "c1", map[string]any{"task_status": st}); err != nil {
			t.Fatalf("patch %s: %v", st, err)
		}
		if got := displayOf(t, db, sid)["task_status"]; got != st {
			t.Fatalf("after patch %s, status = %v", st, got)
		}
	}
}

// under concurrent patches the terminal status wins regardless of
// scheduling (and the read-merge-write races cleanly under -race).
func TestPatchToolCallDisplayConcurrentTerminalWins(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	ms := NewMessageStore(db)
	sid := NewID()

	row := NewItemMessageRaw(sid, "r1", "m", []byte(`{"type":"function_call","call_id":"c1","name":"spawn_task","arguments":"{}"}`))
	if _, err := db.NewInsert().Model(&row).Exec(ctx); err != nil {
		t.Fatalf("insert: %v", err)
	}

	statuses := []string{"working", "input_required", "working", "completed", "input_required", "working"}
	var wg sync.WaitGroup
	for _, st := range statuses {
		wg.Add(1)
		go func(status string) {
			defer wg.Done()
			if err := ms.PatchToolCallDisplay(ctx, sid, "c1", map[string]any{"task_status": status}); err != nil {
				t.Errorf("patch %s: %v", status, err)
			}
		}(st)
	}
	wg.Wait()

	if got := displayOf(t, db, sid)["task_status"]; got != "completed" {
		t.Fatalf("concurrent patches did not converge on terminal: %v", got)
	}
}

// a row whose item can't be deserialized must survive: the delete only
// commits after a successful decode, so a decode failure rolls back.
func TestPopItemRollsBackOnUndecodableRow(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	sid := NewID()
	a := NewSessionAdapter(db, sid)

	good := NewItemMessageRaw(sid, "r", "m", []byte(`{"role":"user","content":"keep me"}`))
	// A newer row with non-empty but undecodable item JSON.
	bad := Message{SessionID: sid, RunID: "r", Kind: MessageKindItem, Role: "user", Content: "x", Item: `{"type":`, CreatedAt: time.Now().UTC()}
	if _, err := db.NewInsert().Model(&good).Exec(ctx); err != nil {
		t.Fatalf("insert good: %v", err)
	}
	if _, err := db.NewInsert().Model(&bad).Exec(ctx); err != nil {
		t.Fatalf("insert bad: %v", err)
	}

	if _, err := a.PopItem(ctx); err == nil {
		t.Fatal("expected an error popping an undecodable row")
	}
	// Neither row was deleted — no silent data loss.
	var remaining []Message
	if err := db.NewSelect().Model(&remaining).Where("session_id = ?", sid).Scan(ctx); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(remaining) != 2 {
		t.Fatalf("rows = %d, want 2 (nothing lost on decode failure)", len(remaining))
	}
}

// the filter matches GetItems: placeholder items ({} / null) are skipped,
// never popped, so PopItem returns the real replayable item beneath them.
func TestPopItemSkipsPlaceholderItems(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	sid := NewID()
	a := NewSessionAdapter(db, sid)

	good := NewItemMessageRaw(sid, "r", "m", []byte(`{"role":"user","content":"real"}`))
	empty := Message{SessionID: sid, RunID: "r", Kind: MessageKindItem, Role: "user", Content: "", Item: `{}`, CreatedAt: time.Now().UTC()}
	null := Message{SessionID: sid, RunID: "r", Kind: MessageKindItem, Role: "user", Content: "", Item: `null`, CreatedAt: time.Now().UTC()}
	if _, err := db.NewInsert().Model(&good).Exec(ctx); err != nil {
		t.Fatalf("insert good: %v", err)
	}
	// Newer placeholder rows that GetItems would skip.
	if _, err := db.NewInsert().Model(&[]Message{empty, null}).Exec(ctx); err != nil {
		t.Fatalf("insert placeholders: %v", err)
	}

	got, err := a.PopItem(ctx)
	if err != nil {
		t.Fatalf("pop: %v", err)
	}
	if got == nil {
		t.Fatal("expected the real item, got nil")
	}
	raw, err := agents.MarshalInputItem(*got)
	if err != nil {
		t.Fatalf("marshal popped: %v", err)
	}
	if !strings.Contains(string(raw), "real") {
		t.Fatalf("popped the wrong row: %s", raw)
	}
	// The placeholder rows are untouched; the real item is the only one removed.
	var remaining []Message
	if err := db.NewSelect().Model(&remaining).Where("session_id = ?", sid).Scan(ctx); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(remaining) != 2 {
		t.Fatalf("remaining = %d, want 2 (both placeholders kept)", len(remaining))
	}
	for _, m := range remaining {
		if m.Item != "{}" && m.Item != "null" {
			t.Fatalf("PopItem deleted a placeholder instead of the real item: %+v", m)
		}
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

// deleting an agent config deletes its scoped memories and clears the
// dangling agent_config_id from sessions and tasks, while leaving global and
// other agents' memories intact.
func TestAgentConfigDeleteCleansReferences(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	agentConfigs := NewAgentConfigStore(db)
	memories := NewMemoryStore(db)
	sessions := NewSessionStore(db)
	tasks := NewTaskStore(db)

	agent := &AgentConfig{Name: "doomed"}
	if err := agentConfigs.Create(ctx, agent); err != nil {
		t.Fatalf("create agent: %v", err)
	}

	scoped := &Memory{AgentConfigID: agent.ID, Key: "s", Content: "scoped"}
	global := &Memory{Key: "g", Content: "global"}
	other := &Memory{AgentConfigID: "other-agent", Key: "o", Content: "other"}
	for _, m := range []*Memory{scoped, global, other} {
		if err := memories.Create(ctx, m); err != nil {
			t.Fatalf("create memory: %v", err)
		}
	}

	boundSess := &Session{ID: NewID(), Name: "bound", AgentConfigID: agent.ID}
	if err := sessions.Create(ctx, boundSess); err != nil {
		t.Fatalf("create session: %v", err)
	}
	boundTask := &Task{ID: NewID(), RunID: NewID(), ParentSessionID: boundSess.ID, ChildSessionID: NewID(), AgentConfigID: agent.ID, Status: "working"}
	if err := tasks.Create(ctx, boundTask); err != nil {
		t.Fatalf("create task: %v", err)
	}

	if err := agentConfigs.Delete(ctx, agent.ID); err != nil {
		t.Fatalf("delete agent: %v", err)
	}

	// Agent gone.
	if _, err := agentConfigs.Get(ctx, agent.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("agent should be deleted, got %v", err)
	}
	// Scoped memory gone; global + other kept.
	remaining, err := memories.List(ctx)
	if err != nil {
		t.Fatalf("list memories: %v", err)
	}
	keys := map[string]bool{}
	for _, m := range remaining {
		keys[m.Key] = true
	}
	if keys["s"] {
		t.Fatalf("scoped memory should be deleted with its agent")
	}
	if !keys["g"] || !keys["o"] {
		t.Fatalf("global/other memories should survive: %+v", remaining)
	}
	// Session and task keep their history but lose the dangling binding.
	gotSess, err := sessions.Get(ctx, boundSess.ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if gotSess.AgentConfigID != "" {
		t.Fatalf("session agent_config_id should be cleared, got %q", gotSess.AgentConfigID)
	}
	gotTask, err := tasks.Get(ctx, boundTask.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if gotTask.AgentConfigID != "" {
		t.Fatalf("task agent_config_id should be cleared, got %q", gotTask.AgentConfigID)
	}
}

// a missing agent config still reports ErrNotFound, and non-agent CRUD
// deletes are unaffected by the cascade gate.
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

// persistCompaction must not insert an orphan summary when the messages it
// planned to compact were deleted out from under it.
func TestPersistCompactionSkipsWhenMessagesGone(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	sessionID := NewID()
	insertItemRows(t, db, sessionID, []string{userItemJSON, assistantItemJSON})
	msgs := loadMessages(t, db, sessionID)
	ids := []int64{msgs[0].ID, msgs[1].ID}

	summary, err := NewItemMessage(sessionID, "r1", "m", responsesSummaryItem(t, "sum"))
	if err != nil {
		t.Fatalf("summary item: %v", err)
	}
	summary.Role = "compaction"
	ca := NewCompactionAdapter(NewSessionAdapter(db, sessionID), &summaryFakeModel{}, 1, 1, "", CompactionNotifier{})

	// Simulate a concurrent session delete: the target messages are gone.
	if _, err := db.NewDelete().Model((*Message)(nil)).Where("session_id = ?", sessionID).Exec(ctx); err != nil {
		t.Fatalf("delete messages: %v", err)
	}

	applied, err := ca.persistCompaction(ctx, ids, &summary)
	if err != nil {
		t.Fatalf("persistCompaction: %v", err)
	}
	if applied {
		t.Fatal("compaction must not apply when target rows are gone")
	}
	if n := len(loadMessages(t, db, sessionID)); n != 0 {
		t.Fatalf("orphan summary inserted into a vanished session: %d rows", n)
	}

	// Positive control: with the rows present it applies and inserts the summary.
	insertItemRows(t, db, sessionID, []string{userItemJSON, assistantItemJSON})
	got := loadMessages(t, db, sessionID)
	ids2 := []int64{got[0].ID, got[1].ID}
	summary2, err := NewItemMessage(sessionID, "r1", "m", responsesSummaryItem(t, "sum2"))
	if err != nil {
		t.Fatalf("summary2 item: %v", err)
	}
	summary2.Role = "compaction"
	applied, err = ca.persistCompaction(ctx, ids2, &summary2)
	if err != nil {
		t.Fatalf("persistCompaction (positive): %v", err)
	}
	if !applied {
		t.Fatal("compaction should apply when target rows exist")
	}
	after := loadMessages(t, db, sessionID)
	if len(after) != 3 {
		t.Fatalf("want 3 rows (2 compacted + summary), got %d", len(after))
	}
}

// ForkMessages copies the source snapshot atomically, dedupes run ids, and
// leaves the source untouched.
func TestForkMessagesCopiesSnapshot(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	ms := NewMessageStore(db)

	rows := []Message{
		NewItemMessageRaw("src", "runA", "m", []byte(`{"role":"user","content":"1"}`)),
		NewItemMessageRaw("src", "runA", "m", []byte(`{"role":"user","content":"2"}`)),
		NewItemMessageRaw("src", "runB", "m", []byte(`{"role":"user","content":"3"}`)),
	}
	if _, err := db.NewInsert().Model(&rows).Exec(ctx); err != nil {
		t.Fatalf("insert: %v", err)
	}

	runIDs, err := ms.ForkMessages(ctx, "src", "dst", 0, false)
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	if len(runIDs) != 2 {
		t.Fatalf("run ids = %v, want 2 deduped", runIDs)
	}
	dst, err := ms.GetMessages(ctx, "dst", 0, 0)
	if err != nil {
		t.Fatalf("get dst: %v", err)
	}
	if len(dst) != 3 {
		t.Fatalf("dst copied %d rows, want 3", len(dst))
	}
	src, err := ms.GetMessages(ctx, "src", 0, 0)
	if err != nil {
		t.Fatalf("get src: %v", err)
	}
	if len(src) != 3 {
		t.Fatalf("src changed by fork: %d rows", len(src))
	}

	// Empty source forks to nothing, no error.
	ids, err := ms.ForkMessages(ctx, "nonexistent", "dst2", 0, false)
	if err != nil || ids != nil {
		t.Fatalf("empty fork: ids=%v err=%v, want nil,nil", ids, err)
	}
}

// the upToMessageID boundary is honored (inclusive vs exclusive).
func TestForkMessagesUpToBoundary(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	ms := NewMessageStore(db)

	rows := []Message{
		NewItemMessageRaw("src", "r", "m", []byte(`{"role":"user","content":"1"}`)),
		NewItemMessageRaw("src", "r", "m", []byte(`{"role":"user","content":"2"}`)),
		NewItemMessageRaw("src", "r", "m", []byte(`{"role":"user","content":"3"}`)),
	}
	if _, err := db.NewInsert().Model(&rows).Exec(ctx); err != nil {
		t.Fatalf("insert: %v", err)
	}
	all, err := ms.GetMessages(ctx, "src", 0, 0)
	if err != nil {
		t.Fatalf("get src: %v", err)
	}
	cut := all[1].ID

	if _, err := ms.ForkMessages(ctx, "src", "inc", cut, false); err != nil {
		t.Fatalf("inclusive fork: %v", err)
	}
	inc, _ := ms.GetMessages(ctx, "inc", 0, 0)
	if len(inc) != 2 {
		t.Fatalf("inclusive up-to copied %d, want 2", len(inc))
	}

	if _, err := ms.ForkMessages(ctx, "src", "exc", cut, true); err != nil {
		t.Fatalf("exclusive fork: %v", err)
	}
	exc, _ := ms.GetMessages(ctx, "exc", 0, 0)
	if len(exc) != 1 {
		t.Fatalf("exclusive up-to copied %d, want 1", len(exc))
	}
}
