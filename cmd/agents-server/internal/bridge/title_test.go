package bridge

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

func insertUserMsg(t *testing.T, r *Runner, sessionID, text string) {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"role": "user", "content": text})
	m := store.NewItemMessageRaw(sessionID, "run", "gpt-test", raw)
	if _, err := r.db.NewInsert().Model(&m).Exec(context.Background()); err != nil {
		t.Fatalf("insert user msg: %v", err)
	}
}

func insertAssistantMsg(t *testing.T, r *Runner, sessionID, text string) {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{
		"type":    "message",
		"role":    "assistant",
		"content": []map[string]string{{"type": "output_text", "text": text}},
	})
	m := store.NewItemMessageRaw(sessionID, "run", "gpt-test", raw)
	if _, err := r.db.NewInsert().Model(&m).Exec(context.Background()); err != nil {
		t.Fatalf("insert assistant msg: %v", err)
	}
}

// firstUserMessage is the seam that lets title generation run from either the
// initial-run or the HITL-resume completion path (Plan A): it sources the title
// seed from the persisted session rather than a passed-in string, so the resume
// path — which never held the original input — can still name the session.
func TestFirstUserMessage(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	runner := NewRunner(ctx, db, &AgentDeps{})

	sid := store.NewID()

	// No messages yet -> empty (title gen bails).
	if got := runner.firstUserMessage(ctx, sid); got != "" {
		t.Fatalf("empty session: got %q, want \"\"", got)
	}

	// An assistant message precedes the first user turn; the earliest *user*
	// message must be chosen, not the earliest message.
	insertAssistantMsg(t, runner, sid, "greetings")
	insertUserMsg(t, runner, sid, "first question")
	insertUserMsg(t, runner, sid, "second question")

	if got := runner.firstUserMessage(ctx, sid); got != "first question" {
		t.Fatalf("got %q, want %q", got, "first question")
	}

	// Scoped to the session — another session's messages don't bleed in.
	other := store.NewID()
	if got := runner.firstUserMessage(ctx, other); got != "" {
		t.Fatalf("other session: got %q, want \"\"", got)
	}
}

// A session that is no longer named "New Chat" must never be renamed, and a
// "New Chat" with no user message yet must be left alone — the two guards that
// keep the (now dual-path) title generation from misfiring.
func TestMaybeGenerateTitleGuards(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	sessions := store.NewSessionStore(db)
	runner := NewRunner(ctx, db, &AgentDeps{Sessions: sessions})

	// Already-titled session: no-op even with a user message present.
	titled := &store.Session{ID: store.NewID(), Name: "Existing Title"}
	if err := sessions.Create(ctx, titled); err != nil {
		t.Fatalf("create titled session: %v", err)
	}
	insertUserMsg(t, runner, titled.ID, "hello there")
	runner.maybeGenerateTitle(ctx, titled.ID, "", func(string, any) {})
	if got, _ := sessions.Get(ctx, titled.ID); got.Name != "Existing Title" {
		t.Errorf("titled session renamed to %q", got.Name)
	}

	// New Chat with no user message: no-op (bails before touching a model).
	empty := &store.Session{ID: store.NewID(), Name: "New Chat"}
	if err := sessions.Create(ctx, empty); err != nil {
		t.Fatalf("create empty session: %v", err)
	}
	runner.maybeGenerateTitle(ctx, empty.ID, "", func(string, any) {})
	if got, _ := sessions.Get(ctx, empty.ID); got.Name != "New Chat" {
		t.Errorf("empty New Chat renamed to %q", got.Name)
	}
}
