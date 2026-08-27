package bridge

import (
	"context"
	"errors"
	"testing"

	"github.com/uptrace/bun"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/cmd/agents-server/internal/settings"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/cmd/agents-server/internal/testdb"
)

func newBareRunner(t *testing.T) (*Runner, *bun.DB) {
	t.Helper()
	db := testdb.New(t)
	runner := NewRunner(context.Background(), db, &AgentDeps{
		AgentConfigs:     store.NewAgentConfigStore(db),
		Sessions:         store.NewSessionStore(db),
		Settings:         settings.NewReader(store.NewSettingStore(db)),
		Memories:         store.NewMemoryStore(db),
		PendingApprovals: store.NewPendingApprovalStore(db),
		Targets:          store.NewSandboxTargetStore(db),
		Templates:        store.NewSandboxTemplateStore(db),
		Projects:         store.NewProjectStore(db),
	})
	return runner, db
}

// countDisplays counts a session's entries of one kind whose display kind
// matches — the entry-model equivalent of the old (kind, role) row count.
func countDisplays(t *testing.T, db *bun.DB, sid string, kind session.EntryKind, display string) int {
	t.Helper()
	entries, err := mustStore(t, db, sid).Entries(context.Background(), session.Cursor{})
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
	if err := store.NewSessionStore(db).Create(context.Background(), &store.Session{OwnerID: store.LocalUserID, ID: sid, Name: "s"}); err != nil {
		t.Fatalf("create session: %v", err)
	}

	// Simulate the SDK having persisted the user input for this run.
	es := mustStore(t, db, sid)
	es.SetRunID(rid)
	es.SetModel("m")
	userItem, err := session.NewItemEntry(agents.InputItemsFromText("hello")[0], agents.Source{Type: agents.SourceUser})
	if err != nil {
		t.Fatalf("build user item: %v", err)
	}
	if err := es.Append(context.Background(), userItem); err != nil {
		t.Fatalf("seed user item: %v", err)
	}

	runner.savePartialTurn(partialTurn{sessionID: sid, runID: rid, model: "m", userInput: "hello", annRole: "cancelled"})

	if got := countDisplays(t, db, sid, session.EntryKindAnnotation, agents.DisplayCancelled); got != 1 {
		t.Errorf("cancelled annotations = %d, want 1", got)
	}
	// The prompt must not be re-inserted — still exactly one user item entry.
	if got := countDisplays(t, db, sid, session.EntryKindItem, ""); got != 1 {
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
	if err := store.NewSessionStore(db).Create(context.Background(), &store.Session{OwnerID: store.LocalUserID, ID: sid, Name: "s"}); err != nil {
		t.Fatalf("create session: %v", err)
	}

	runner.savePartialTurn(partialTurn{
		sessionID:        sid,
		runID:            rid,
		model:            "m",
		userInput:        "hello",
		annRole:          "cancelled",
		partialReasoning: "let me think about primes",
		partialText:      "here is my parti",
	})

	if got := countDisplays(t, db, sid, session.EntryKindAnnotation, agents.DisplayReasoning); got != 1 {
		t.Errorf("reasoning annotations = %d, want 1", got)
	}
	if got := countDisplays(t, db, sid, session.EntryKindAnnotation, agents.DisplayMessage); got != 1 {
		t.Errorf("partial-text annotations = %d, want 1", got)
	}
	if got := countDisplays(t, db, sid, session.EntryKindAnnotation, agents.DisplayCancelled); got != 1 {
		t.Errorf("cancelled annotations = %d, want 1", got)
	}
	// Display-only: none of these annotations may be replayed to the model.
	items, err := session.NewSession(mustStore(t, db, sid)).ContextItems(context.Background(), session.Cursor{})
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
	if err := store.NewSessionStore(db).Create(context.Background(), &store.Session{OwnerID: store.LocalUserID, ID: sid, Name: "s"}); err != nil {
		t.Fatalf("create session: %v", err)
	}

	runner.savePartialTurn(partialTurn{sessionID: sid, runID: rid, model: "m", userInput: "hello", annRole: "cancelled"})

	if got := countDisplays(t, db, sid, session.EntryKindItem, ""); got != 1 {
		t.Errorf("user fallback entries = %d, want 1", got)
	}
	if got := countDisplays(t, db, sid, session.EntryKindAnnotation, agents.DisplayCancelled); got != 1 {
		t.Errorf("cancelled annotations = %d, want 1", got)
	}
}

// The prompt fallback asks whether THIS generation of the session already holds
// the run's items. A session deleted and recreated under the same id is a
// different session, and a "yes" from the generation it replaced would make the
// save skip the prompt it is the only writer of.
func TestSavePartialTurn_IgnoresASupersededGeneration(t *testing.T) {
	runner, db := newBareRunner(t)
	sid, rid := store.NewID(), store.NewID()
	if err := store.NewSessionStore(db).Create(context.Background(), &store.Session{OwnerID: store.LocalUserID, ID: sid, Name: "s"}); err != nil {
		t.Fatalf("create session: %v", err)
	}

	dead := mustStore(t, db, sid)
	dead.SetRunID(rid)
	dead.SetModel("m")
	userItem, err := session.NewItemEntry(agents.InputItemsFromText("hello")[0], agents.Source{Type: agents.SourceUser})
	if err != nil {
		t.Fatalf("build user item: %v", err)
	}
	if err := dead.Append(context.Background(), userItem); err != nil {
		t.Fatalf("seed the superseded generation: %v", err)
	}

	// Rotate the row's generation, leaving the entries above behind: the id now
	// answers for a different session, as it does after a delete and a recreate
	// under the same name.
	if _, err := db.NewUpdate().Model((*store.Session)(nil)).
		Set("gen = ?", store.NewID()).Where("id = ?", sid).
		Exec(context.Background()); err != nil {
		t.Fatalf("rotate generation: %v", err)
	}

	runner.savePartialTurn(partialTurn{sessionID: sid, runID: rid, model: "m", userInput: "hello", annRole: "cancelled"})

	if got := countDisplays(t, db, sid, session.EntryKindItem, ""); got != 1 {
		t.Errorf("user fallback entries = %d, want 1 (the live generation has no prompt of its own)", got)
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
