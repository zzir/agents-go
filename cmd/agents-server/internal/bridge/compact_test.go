package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// summaryModelServer answers non-streaming Responses calls with one fixed
// assistant message — the summarization request a manual compaction makes.
func summaryModelServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "resp_sum", "object": "response", "created_at": 0, "status": "completed", "model": "gpt-test",
			"output": []any{map[string]any{
				"type": "message", "id": "msg_sum", "status": "completed", "role": "assistant",
				"content": []any{map[string]any{"type": "output_text", "text": "what came before", "annotations": []any{}}},
			}},
			"usage": map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2},
		})
	}))
}

func itemEntry(raw string, src agents.SourceType) session.Entry {
	return session.Entry{Kind: session.EntryKindItem, Source: agents.Source{Type: src}, Item: json.RawMessage(raw)}
}

// TestCompactSessionFoldsOutsideARun locks the manual pass end to end: it
// resolves the summary model from the config, folds the active prefix past the
// kept window, reports the counts — and a second pass with nothing left to
// fold answers compacted=false rather than an error.
func TestCompactSessionFoldsOutsideARun(t *testing.T) {
	ctx := context.Background()
	srv := summaryModelServer(t)
	defer srv.Close()

	runner, sessions, _, agentConfigs := newTaskTestRunner(t)
	ac := &store.AgentConfig{
		Name:       "worker",
		Model:      "gpt-test",
		ProviderID: testProvider(t, runner.db, "sum", "k", srv.URL),
		Compaction: store.CompactionGroup{Enabled: true, Threshold: 1_000_000, Window: 1},
	}
	if err := agentConfigs.Create(ctx, ac); err != nil {
		t.Fatal(err)
	}
	sess := &store.Session{OwnerID: store.LocalUserID, ID: store.NewID(), Name: "chat", AgentConfigID: ac.ID}
	if err := sessions.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	ref, err := store.RefFor(ctx, runner.db, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.NewEntryStoreFor(runner.db, ref).Append(ctx,
		itemEntry(`{"role":"user","content":"question one"}`, agents.SourceUser),
		itemEntry(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer one"}]}`, agents.SourceModel),
		itemEntry(`{"role":"user","content":"question two"}`, agents.SourceUser),
	); err != nil {
		t.Fatal(err)
	}

	compacted, before, after, err := runner.CompactSession(ctx, sess.ID)
	if err != nil {
		t.Fatalf("CompactSession: %v", err)
	}
	if !compacted || before != 3 || after != 2 {
		t.Fatalf("want a 3→2 fold, got compacted=%v before=%d after=%d", compacted, before, after)
	}

	// A session the kept window already covers: a polite no-op, not an error.
	short := &store.Session{OwnerID: store.LocalUserID, ID: store.NewID(), Name: "short", AgentConfigID: ac.ID}
	if err := sessions.Create(ctx, short); err != nil {
		t.Fatal(err)
	}
	shortRef, err := store.RefFor(ctx, runner.db, short.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.NewEntryStoreFor(runner.db, shortRef).Append(ctx,
		itemEntry(`{"role":"user","content":"only one"}`, agents.SourceUser),
	); err != nil {
		t.Fatal(err)
	}
	compacted, _, _, err = runner.CompactSession(ctx, short.ID)
	if err != nil {
		t.Fatalf("CompactSession (nothing to fold): %v", err)
	}
	if compacted {
		t.Fatal("a pass with nothing to fold must answer compacted=false")
	}
}

// The busy check and the config gate answer with their typed errors, so the
// handler can map them to 409 and 400 instead of a masked 500.
func TestCompactSessionGuards(t *testing.T) {
	ctx := context.Background()
	runner, sessions, _, agentConfigs := newTaskTestRunner(t)

	// Compaction disabled on the agent.
	ac := &store.AgentConfig{
		Name:       "worker",
		Model:      "gpt-test",
		ProviderID: testProvider(t, runner.db, "keyed", "k", ""),
	}
	if err := agentConfigs.Create(ctx, ac); err != nil {
		t.Fatal(err)
	}
	sess := &store.Session{OwnerID: store.LocalUserID, ID: store.NewID(), Name: "chat", AgentConfigID: ac.ID}
	if err := sessions.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := runner.CompactSession(ctx, sess.ID); !errors.Is(err, ErrCompactionUnavailable) {
		t.Fatalf("disabled compaction should answer ErrCompactionUnavailable, got %v", err)
	}

	// A live run on the session wins over the pass.
	seg, _, err := runner.hub.register("run_live", sess.ID, "", ac.ID, "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer runner.hub.unregister("run_live", seg)
	if _, _, _, err := runner.CompactSession(ctx, sess.ID); !errors.As(err, &ErrSessionBusy{}) {
		t.Fatalf("a live run should answer ErrSessionBusy, got %v", err)
	}
}
