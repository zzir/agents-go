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

// countDisplays counts a session's entries of one kind whose display kind
// matches — the entry-model equivalent of the old (kind, role) row count.
func countDisplays(t *testing.T, db *bun.DB, sid string, kind agents.EntryKind, display string) int {
	t.Helper()
	entries, err := mustStore(t, db, sid).AllEntries(context.Background())
	if err != nil {
		t.Fatalf("load entries: %v", err)
	}
	n := 0
	for _, e := range entries {
		if e.Kind != kind {
			continue
		}
		if display == "" || (e.Display != nil && e.Display.Kind == display) {
			n++
		}
	}
	return n
}

// A run that already persisted items (the SDK's per-turn save) gets only the
// cancelled annotation — the prompt is not duplicated.
func TestSavePartialTurn_CancelledAfterItems(t *testing.T) {
	runner, db := newBareRunner(t)
	sid, rid := store.NewID(), store.NewID()
	// A session row, because that is what a session id addresses: the runner
	// resolves the generation before it writes, and an id with no row is not a
	// session it can write to.
	if err := store.NewSessionStore(db).Create(context.Background(), &store.Session{ID: sid, Name: "s"}); err != nil {
		t.Fatalf("create session: %v", err)
	}

	// Simulate the SDK having persisted the user input for this run.
	es := mustStore(t, db, sid)
	es.SetRunID(rid)
	es.SetModel("m")
	userItem, err := agents.NewItemEntry(agents.InputItemsFromText("hello")[0], agents.Source{Type: agents.SourceUser})
	if err != nil {
		t.Fatalf("build user item: %v", err)
	}
	if err := es.Append(context.Background(), userItem); err != nil {
		t.Fatalf("seed user item: %v", err)
	}

	runner.savePartialTurn(sid, rid, "m", "hello", "cancelled", "", "", "", "", "")

	if got := countDisplays(t, db, sid, agents.EntryKindAnnotation, agents.DisplayCancelled); got != 1 {
		t.Errorf("cancelled annotations = %d, want 1", got)
	}
	// The prompt must not be re-inserted — still exactly one user item entry.
	if got := countDisplays(t, db, sid, agents.EntryKindItem, ""); got != 1 {
		t.Errorf("user item entries = %d, want 1 (no duplicate)", got)
	}
}

// A cancel during the thinking phase (an in-flight turn the SDK never saved)
// keeps the streamed reasoning and text as display-only annotations, so the
// step content does not vanish — leaving only the user's prompt.
func TestSavePartialTurn_KeepsInFlightThinking(t *testing.T) {
	runner, db := newBareRunner(t)
	sid, rid := store.NewID(), store.NewID()
	// A session row, because that is what a session id addresses: the runner
	// resolves the generation before it writes, and an id with no row is not a
	// session it can write to.
	if err := store.NewSessionStore(db).Create(context.Background(), &store.Session{ID: sid, Name: "s"}); err != nil {
		t.Fatalf("create session: %v", err)
	}

	runner.savePartialTurn(sid, rid, "m", "hello", "cancelled", "", "let me think about primes", "here is my parti", "", "")

	if got := countDisplays(t, db, sid, agents.EntryKindAnnotation, agents.DisplayReasoning); got != 1 {
		t.Errorf("reasoning annotations = %d, want 1", got)
	}
	if got := countDisplays(t, db, sid, agents.EntryKindAnnotation, agents.DisplayMessage); got != 1 {
		t.Errorf("partial-text annotations = %d, want 1", got)
	}
	if got := countDisplays(t, db, sid, agents.EntryKindAnnotation, agents.DisplayCancelled); got != 1 {
		t.Errorf("cancelled annotations = %d, want 1", got)
	}
	// Display-only: none of these annotations may be replayed to the model.
	items, err := agents.NewSession(mustStore(t, db, sid)).ContextItems(context.Background(), agents.Cursor{})
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
	// A session row, because that is what a session id addresses: the runner
	// resolves the generation before it writes, and an id with no row is not a
	// session it can write to.
	if err := store.NewSessionStore(db).Create(context.Background(), &store.Session{ID: sid, Name: "s"}); err != nil {
		t.Fatalf("create session: %v", err)
	}

	runner.savePartialTurn(sid, rid, "m", "hello", "cancelled", "", "", "", "", "")

	if got := countDisplays(t, db, sid, agents.EntryKindItem, ""); got != 1 {
		t.Errorf("user fallback entries = %d, want 1", got)
	}
	if got := countDisplays(t, db, sid, agents.EntryKindAnnotation, agents.DisplayCancelled); got != 1 {
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

// mustStore addresses a session by resolving its generation, the way the code
// under test does.
func mustStore(t *testing.T, db *bun.DB, id string) *store.EntryStore {
	t.Helper()
	ref, err := store.RefFor(context.Background(), db, id)
	if err != nil {
		t.Fatalf("resolving session %s: %v", id, err)
	}
	return store.NewEntryStoreFor(db, ref)
}
