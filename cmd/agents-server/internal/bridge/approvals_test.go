package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/uptrace/bun"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

func newTestDB(t *testing.T) *bun.DB {
	t.Helper()
	db, err := store.NewSQLiteDB("file:" + store.NewID() + "?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.CreateSchema(context.Background(), db); err != nil {
		t.Fatalf("schema: %v", err)
	}
	return db
}

// TestResolveApprovalBusyKeepsPending locks the claim/restore semantics: when
// the session already has a live run, resolving an approval must fail with
// ErrSessionBusy AND leave the pending row in place so the decision can be
// retried — losing the row would strand the paused run forever.
func TestResolveApprovalBusyKeepsPending(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	agentConfigs := store.NewAgentConfigStore(db)
	approvals := store.NewPendingApprovalStore(db)
	sessions := store.NewSessionStore(db)

	// The agent config the registry is rebuilt from; its Name must match the
	// serialized state's current agent.
	ac := &store.AgentConfig{Name: "approver", Model: "gpt-test"}
	if err := agentConfigs.Create(ctx, ac); err != nil {
		t.Fatalf("create agent config: %v", err)
	}
	sess := &store.Session{ID: store.NewID(), Name: "s"}
	if err := sessions.Create(ctx, sess); err != nil {
		t.Fatalf("create session: %v", err)
	}

	runner := NewRunner(ctx, db, &AgentDeps{
		AgentConfigs:     agentConfigs,
		Sessions:         sessions,
		Settings:         store.NewSettingStore(db),
		Memories:         store.NewMemoryStore(db),
		PendingApprovals: approvals,
	})

	// Serialize a real interrupted RunState so ResolveApproval exercises the
	// genuine restore path. Raw must be a valid output item — the state
	// serializer round-trips its wire JSON.
	var rawCall agents.TResponseOutputItem
	if err := json.Unmarshal([]byte(`{"type":"function_call","call_id":"call-busy-1","name":"shell","arguments":"{}"}`), &rawCall); err != nil {
		t.Fatalf("unmarshal raw call: %v", err)
	}
	state := &agents.RunState{
		CurrentAgent: &agents.Agent{Name: "approver"},
		Approvals:    agents.NewApprovalStore(),
		Interruptions: []*agents.ToolApprovalItem{{
			Agent:    &agents.Agent{Name: "approver"},
			ToolName: "shell",
			CallID:   "call-busy-1",
			Raw:      rawCall,
		}},
	}
	stateJSON, err := state.MarshalJSON()
	if err != nil {
		t.Fatalf("marshal run state: %v", err)
	}
	calls, _ := json.Marshal([]store.PendingToolCall{{ToolCallID: "call-busy-1", ToolName: "shell"}})
	if err := approvals.Save(ctx, &store.PendingApproval{
		RunID:         "paused-run",
		SessionID:     sess.ID,
		AgentConfigID: ac.ID,
		State:         string(stateJSON),
		ToolCalls:     calls,
	}); err != nil {
		t.Fatalf("save pending: %v", err)
	}

	// Occupy the session with a live run so ResumeRun must fail busy.
	if _, _, err := runner.hub.register("blocker-run", sess.ID, ac.ID, "", nil); err != nil {
		t.Fatalf("register blocker: %v", err)
	}

	_, _, err = runner.ResolveApproval(ctx, "call-busy-1", true, ApprovalOnce, "", nil)
	var busy ErrSessionBusy
	if !errors.As(err, &busy) {
		t.Fatalf("want ErrSessionBusy, got %v", err)
	}
	if _, err := approvals.Get(ctx, "paused-run"); err != nil {
		t.Fatalf("pending row must survive a busy failure, got %v", err)
	}

	// Once the session frees up, the same decision goes through and consumes
	// the row.
	runner.hub.finish("blocker-run", false)
	runID, _, err := runner.ResolveApproval(ctx, "call-busy-1", true, ApprovalOnce, "", nil)
	if err != nil {
		t.Fatalf("resolve after free: %v", err)
	}
	if runID == "" {
		t.Fatal("expected a continuation run id")
	}
	if _, err := approvals.Get(ctx, "paused-run"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("pending row should be consumed, got %v", err)
	}

	// A second decision for the same call is not found (single execution).
	if _, _, err := runner.ResolveApproval(ctx, "call-busy-1", true, ApprovalOnce, "", nil); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second resolve: want ErrNotFound, got %v", err)
	}
}

// A pending approval whose serialized RunState predates the current schema
// version is unresumable: ResolveApproval reports a StaleApprovalStateError and
// discards the row so it can't wedge the session with a masked 500 on retry.
func TestResolveApprovalStaleSchemaDiscarded(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	agentConfigs := store.NewAgentConfigStore(db)
	approvals := store.NewPendingApprovalStore(db)
	sessions := store.NewSessionStore(db)

	ac := &store.AgentConfig{Name: "approver", Model: "gpt-test"}
	if err := agentConfigs.Create(ctx, ac); err != nil {
		t.Fatalf("create agent config: %v", err)
	}
	sess := &store.Session{ID: store.NewID(), Name: "s"}
	if err := sessions.Create(ctx, sess); err != nil {
		t.Fatalf("create session: %v", err)
	}
	runner := NewRunner(ctx, db, &AgentDeps{
		AgentConfigs:     agentConfigs,
		Sessions:         sessions,
		Settings:         store.NewSettingStore(db),
		Memories:         store.NewMemoryStore(db),
		PendingApprovals: approvals,
	})

	// A state stamped with an older schema version — the current binary can
	// never decode it (RunStateFromJSON enforces strict version equality).
	staleState := `{"schema_version":"1.1","current_agent":"approver","interruptions":[]}`
	calls, _ := json.Marshal([]store.PendingToolCall{{ToolCallID: "call-old", ToolName: "shell"}})
	if err := approvals.Save(ctx, &store.PendingApproval{
		RunID:         "paused-old",
		SessionID:     sess.ID,
		AgentConfigID: ac.ID,
		State:         staleState,
		ToolCalls:     calls,
	}); err != nil {
		t.Fatalf("save pending: %v", err)
	}

	_, _, err := runner.ResolveApproval(ctx, "call-old", true, ApprovalOnce, "", nil)
	var stale *StaleApprovalStateError
	if !errors.As(err, &stale) {
		t.Fatalf("want *StaleApprovalStateError, got %v", err)
	}
	if stale.HaveVersion != "1.1" {
		t.Errorf("HaveVersion = %q, want 1.1", stale.HaveVersion)
	}
	if _, err := approvals.Get(ctx, "paused-old"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("stale pending row should be discarded, got %v", err)
	}
}
