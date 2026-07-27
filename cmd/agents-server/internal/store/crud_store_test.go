package store

import (
	"context"
	"errors"
	"testing"

	"github.com/uptrace/bun"
)

func newTestDB(t *testing.T) *bun.DB {
	t.Helper()
	db, err := NewSQLiteDB("file:" + NewID() + "?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := CreateSchema(context.Background(), db); err != nil {
		t.Fatalf("schema: %v", err)
	}
	return db
}

func TestCrudStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := NewAgentConfigStore(newTestDB(t))

	// Create stamps id + timestamps via the BeforeAppendModel hook.
	ac := &AgentConfig{Name: "first", Model: "gpt-5.5", Behavior: BehaviorGroup{MaxTurns: 5}, Resilience: ResilienceGroup{RetryEnabled: true}}
	if err := s.Create(ctx, ac); err != nil {
		t.Fatalf("create: %v", err)
	}
	if ac.ID == "" || ac.CreatedAt.IsZero() || ac.UpdatedAt.IsZero() {
		t.Fatalf("create did not stamp id/timestamps: %+v", ac)
	}

	got, err := s.Get(ctx, ac.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "first" || got.Behavior.MaxTurns != 5 || !got.Resilience.RetryEnabled {
		t.Fatalf("get mismatch: %+v", got)
	}

	// Update is a full-row replace except id/created_at; updated_at is refreshed.
	upd := &AgentConfig{Name: "second", Model: "o4-mini", Behavior: BehaviorGroup{MaxTurns: 9}}
	if err := s.Update(ctx, ac.ID, upd); err != nil {
		t.Fatalf("update: %v", err)
	}
	got2, err := s.Get(ctx, ac.ID)
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if got2.Name != "second" || got2.Model != "o4-mini" || got2.Behavior.MaxTurns != 9 {
		t.Fatalf("update not applied: %+v", got2)
	}
	if got2.Resilience.RetryEnabled {
		t.Fatalf("update did not clear RetryEnabled (full-row replace expected)")
	}
	if !got2.CreatedAt.Equal(got.CreatedAt) {
		t.Fatalf("created_at must be immutable: %v -> %v", got.CreatedAt, got2.CreatedAt)
	}
	if got2.UpdatedAt.Before(got.UpdatedAt) {
		t.Fatalf("updated_at went backwards: %v -> %v", got.UpdatedAt, got2.UpdatedAt)
	}

	list, err := s.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 row, got %d", len(list))
	}

	if err := s.Delete(ctx, ac.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.Get(ctx, ac.ID); err == nil {
		t.Fatalf("expected error getting deleted row")
	}
}

func TestMemoryStoreListForAgent(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore(newTestDB(t))

	global := &Memory{Key: "g", Content: "global"}
	scoped := &Memory{AgentConfigID: "agent-1", Key: "s", Content: "scoped"}
	other := &Memory{AgentConfigID: "agent-2", Key: "o", Content: "other"}
	for _, m := range []*Memory{global, scoped, other} {
		if err := s.Create(ctx, m); err != nil {
			t.Fatalf("create: %v", err)
		}
	}

	// agent-1 sees global + its own, never agent-2's.
	got, err := s.ListForAgent(ctx, "agent-1")
	if err != nil {
		t.Fatalf("list for agent: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 memories for agent-1, got %d: %+v", len(got), got)
	}
	for _, m := range got {
		if m.AgentConfigID == "agent-2" {
			t.Fatalf("agent-1 leaked agent-2 memory: %+v", m)
		}
	}

	// Empty agent id sees only global memories.
	globalOnly, err := s.ListForAgent(ctx, "")
	if err != nil {
		t.Fatalf("list global: %v", err)
	}
	if len(globalOnly) != 1 || globalOnly[0].Key != "g" {
		t.Fatalf("expected only global memory, got %+v", globalOnly)
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
