package bridge

import (
	"context"
	"errors"
	"testing"

	"github.com/uptrace/bun"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

func newBareRunner(t *testing.T) (*Runner, *bun.DB) {
	t.Helper()
	db := newTestDB(t)
	runner := NewRunner(context.Background(), db, &AgentDeps{
		AgentConfigs:     store.NewAgentConfigStore(db),
		Sessions:         store.NewSessionStore(db),
		Settings:         store.NewSettingStore(db),
		Memories:         store.NewMemoryStore(db),
		PendingApprovals: store.NewPendingApprovalStore(db),
	})
	return runner, db
}

func countRows(t *testing.T, db *bun.DB, sid, kind, role string) int {
	t.Helper()
	n, err := db.NewSelect().Model((*store.Message)(nil)).
		Where("session_id = ?", sid).
		Where("kind = ?", kind).
		Where("role = ?", role).
		Count(context.Background())
	if err != nil {
		t.Fatalf("count rows: %v", err)
	}
	return n
}

// A run that already persisted items (the SDK's per-turn save) gets only the
// cancelled annotation — the prompt is not duplicated.
func TestSavePartialTurn_CancelledAfterItems(t *testing.T) {
	runner, db := newBareRunner(t)
	sid, rid := store.NewID(), store.NewID()

	// Simulate the SDK having persisted the user input for this run.
	sa := store.NewSessionAdapter(db, sid)
	sa.SetRunID(rid)
	sa.SetModel("m")
	userItem := store.NewItemMessageRaw(sid, rid, "m", []byte(`{"role":"user","content":"hello"}`))
	if _, err := db.NewInsert().Model(&userItem).Exec(context.Background()); err != nil {
		t.Fatalf("seed user item: %v", err)
	}

	runner.savePartialTurn(sid, rid, "m", "hello", "cancelled", "", "", "", "", "")

	if got := countRows(t, db, sid, store.MessageKindAnnotation, "cancelled"); got != 1 {
		t.Errorf("cancelled annotations = %d, want 1", got)
	}
	// The prompt must not be re-inserted — still exactly one user item row.
	if got := countRows(t, db, sid, store.MessageKindItem, "user"); got != 1 {
		t.Errorf("user item rows = %d, want 1 (no duplicate)", got)
	}
}

// A cancel during the thinking phase (an in-flight turn the SDK never saved)
// keeps the streamed reasoning and text as display-only annotations, so the
// step content does not vanish — leaving only the user's prompt.
func TestSavePartialTurn_KeepsInFlightThinking(t *testing.T) {
	runner, db := newBareRunner(t)
	sid, rid := store.NewID(), store.NewID()

	runner.savePartialTurn(sid, rid, "m", "hello", "cancelled", "", "let me think about primes", "here is my parti", "", "")

	if got := countRows(t, db, sid, store.MessageKindAnnotation, "reasoning"); got != 1 {
		t.Errorf("reasoning annotations = %d, want 1", got)
	}
	if got := countRows(t, db, sid, store.MessageKindAnnotation, "assistant"); got != 1 {
		t.Errorf("partial-text annotations = %d, want 1", got)
	}
	if got := countRows(t, db, sid, store.MessageKindAnnotation, "cancelled"); got != 1 {
		t.Errorf("cancelled annotations = %d, want 1", got)
	}
	// Display-only: none of these annotations may be replayed to the model.
	items, err := agents.NewSession(store.NewSessionAdapter(db, sid)).ContextItems(context.Background(), agents.Cursor{})
	if err != nil {
		t.Fatalf("get items: %v", err)
	}
	for _, it := range items {
		if it.OfMessage != nil && it.OfMessage.Role == "assistant" {
			t.Error("partial assistant text leaked into replayable items")
		}
	}
}

// A run cancelled before the SDK saved anything falls back to persisting the
// prompt, so it is not lost, alongside the cancelled annotation.
func TestSavePartialTurn_CancelledBeforeAnyItems(t *testing.T) {
	runner, db := newBareRunner(t)
	sid, rid := store.NewID(), store.NewID()

	runner.savePartialTurn(sid, rid, "m", "hello", "cancelled", "", "", "", "", "")

	if got := countRows(t, db, sid, store.MessageKindItem, "user"); got != 1 {
		t.Errorf("user fallback rows = %d, want 1", got)
	}
	if got := countRows(t, db, sid, store.MessageKindAnnotation, "cancelled"); got != 1 {
		t.Errorf("cancelled annotations = %d, want 1", got)
	}
}

func TestIsCancellation(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	live := context.Background()

	cases := []struct {
		name string
		ctx  context.Context
		err  error
		want bool
	}{
		{"ctx cancelled", cancelled, errors.New("boom"), true},
		{"wrapped context.Canceled", live, errors.Join(errors.New("openai responses"), context.Canceled), true},
		{"deadline exceeded", live, context.DeadlineExceeded, true},
		{"plain error", live, errors.New("500 server error"), false},
	}
	for _, tc := range cases {
		if got := isCancellation(tc.ctx, tc.err); got != tc.want {
			t.Errorf("%s: isCancellation = %v, want %v", tc.name, got, tc.want)
		}
	}
}
