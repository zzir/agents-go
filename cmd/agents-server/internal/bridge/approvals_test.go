package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/settings"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/cmd/agents-server/internal/testdb"
)

// TestResolveApprovalBusyKeepsPending locks the claim/restore semantics: when
// the session already has a live run, resolving an approval must fail with
// ErrSessionBusy AND leave the pending row in place so the decision can be
// retried — losing the row would strand the paused run forever.
func TestResolveApprovalBusyKeepsPending(t *testing.T) {
	ctx := context.Background()
	db := testdb.New(t)

	agentConfigs := store.NewAgentConfigStore(db)
	approvals := store.NewPendingApprovalStore(db)
	sessions := store.NewSessionStore(db)

	// The agent config the registry is rebuilt from; its Name must match the
	// serialized state's current agent.
	ac := &store.AgentConfig{OwnerID: store.LocalUserID, Name: "approver", Model: "gpt-test"}
	if err := agentConfigs.Create(ctx, ac); err != nil {
		t.Fatalf("create agent config: %v", err)
	}
	sess := &store.Session{OwnerID: store.LocalUserID, ID: store.NewID(), Name: "s"}
	if err := sessions.Create(ctx, sess); err != nil {
		t.Fatalf("create session: %v", err)
	}

	runner := NewRunner(ctx, db, &AgentDeps{
		AgentConfigs:     agentConfigs,
		Providers:        store.NewProviderStore(db),
		Sessions:         sessions,
		Settings:         settings.NewReader(store.NewSettingStore(db)),
		Memories:         store.NewMemoryStore(db),
		PendingApprovals: approvals,
	})

	// Serialize a real interrupted RunState so ResolveApproval exercises the
	// genuine restore path. Raw must be a valid output item — the state
	// serializer round-trips its wire JSON.
	var rawCall agents.OutputItem
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
	if _, _, err := runner.hub.register("blocker-run", sess.ID, "", ac.ID, "", nil); err != nil {
		t.Fatalf("register blocker: %v", err)
	}

	_, _, err = runner.ResolveApproval(ctx, "call-busy-1", true, ApprovalOnce, "", nil)
	if _, ok := errors.AsType[ErrSessionBusy](err); !ok {
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
	db := testdb.New(t)

	agentConfigs := store.NewAgentConfigStore(db)
	approvals := store.NewPendingApprovalStore(db)
	sessions := store.NewSessionStore(db)

	ac := &store.AgentConfig{OwnerID: store.LocalUserID, Name: "approver", Model: "gpt-test"}
	if err := agentConfigs.Create(ctx, ac); err != nil {
		t.Fatalf("create agent config: %v", err)
	}
	sess := &store.Session{OwnerID: store.LocalUserID, ID: store.NewID(), Name: "s"}
	if err := sessions.Create(ctx, sess); err != nil {
		t.Fatalf("create session: %v", err)
	}
	runner := NewRunner(ctx, db, &AgentDeps{
		AgentConfigs:     agentConfigs,
		Providers:        store.NewProviderStore(db),
		Sessions:         sessions,
		Settings:         settings.NewReader(store.NewSettingStore(db)),
		Memories:         store.NewMemoryStore(db),
		PendingApprovals: approvals,
	})

	// A state stamped below the SDK's decode floor — the current binary can
	// never decode it, whatever else moves.
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

// A state from an older minor INSIDE the SDK's decode window (1.5 under a 1.6
// binary) is resumable, so the pre-flight must not report it stale — and above
// all must not delete it. The gate is the SDK's window, not string equality:
// an equality gate here destroyed states a purely additive SDK bump resumes
// fine, which is exactly the failure this test pins.
func TestResolveApprovalOlderDecodableSchemaNotDiscarded(t *testing.T) {
	ctx := context.Background()
	db := testdb.New(t)

	agentConfigs := store.NewAgentConfigStore(db)
	approvals := store.NewPendingApprovalStore(db)
	sessions := store.NewSessionStore(db)

	ac := &store.AgentConfig{OwnerID: store.LocalUserID, Name: "approver", Model: "gpt-test"}
	if err := agentConfigs.Create(ctx, ac); err != nil {
		t.Fatalf("create agent config: %v", err)
	}
	sess := &store.Session{OwnerID: store.LocalUserID, ID: store.NewID(), Name: "s"}
	if err := sessions.Create(ctx, sess); err != nil {
		t.Fatalf("create session: %v", err)
	}
	runner := NewRunner(ctx, db, &AgentDeps{
		AgentConfigs:     agentConfigs,
		Providers:        store.NewProviderStore(db),
		Sessions:         sessions,
		Settings:         settings.NewReader(store.NewSettingStore(db)),
		Memories:         store.NewMemoryStore(db),
		PendingApprovals: approvals,
	})

	if !agents.RunStateVersionSupported("1.5") {
		t.Fatal("precondition: 1.5 must be inside the SDK's decode window")
	}
	olderState := `{"schema_version":"1.5","current_agent":"approver","current_turn":1,` +
		`"original_input":[],"generated_items":[],"model_responses":[],` +
		`"interrupted_response":null,"interruptions":[]}`
	calls, _ := json.Marshal([]store.PendingToolCall{{ToolCallID: "call-15", ToolName: "shell"}})
	if err := approvals.Save(ctx, &store.PendingApproval{
		RunID:         "paused-15",
		SessionID:     sess.ID,
		AgentConfigID: ac.ID,
		State:         olderState,
		ToolCalls:     calls,
	}); err != nil {
		t.Fatalf("save pending: %v", err)
	}

	// The resolve fails later (the minimal state holds no matching
	// interruption) — what matters is HOW: not as stale, and without
	// consuming the row.
	_, _, err := runner.ResolveApproval(ctx, "call-15", true, ApprovalOnce, "", nil)
	if err == nil {
		t.Fatal("expected an error from the minimal state")
	}
	if _, ok := errors.AsType[*StaleApprovalStateError](err); ok {
		t.Fatalf("a decodable 1.5 state was reported stale: %v", err)
	}
	if _, err := approvals.Get(ctx, "paused-15"); err != nil {
		t.Errorf("the decodable pending row must survive, got %v", err)
	}
}

// TestResolveApprovalTaskNotYetInputRequiredKeepsPending locks: a task
// approval that lands while the task row is still "working" — the fast-approve
// window before the run's postRun marks it input_required — must NOT be lost. A
// failed ReclaimWorking on a still-non-terminal task restores the pending row
// (returning a retryable ApprovalNotReadyError) instead of deleting it and
// stranding the paused run forever, which is what the old code did.
func TestResolveApprovalTaskNotYetInputRequiredKeepsPending(t *testing.T) {
	ctx := context.Background()
	db := testdb.New(t)

	agentConfigs := store.NewAgentConfigStore(db)
	approvals := store.NewPendingApprovalStore(db)
	sessions := store.NewSessionStore(db)
	tasks := store.NewTaskStore(db)

	ac := &store.AgentConfig{OwnerID: store.LocalUserID, Name: "approver", Model: "gpt-test"}
	if err := agentConfigs.Create(ctx, ac); err != nil {
		t.Fatalf("create agent config: %v", err)
	}
	// The task's hidden child session (pending.SessionID points here).
	child := &store.Session{OwnerID: store.LocalUserID, ID: store.NewID(), Name: "task"}
	if err := sessions.Create(ctx, child); err != nil {
		t.Fatalf("create child session: %v", err)
	}
	// A task still recorded as "working": its run interrupted and persisted the
	// approval, but postRun has not yet flipped it to input_required.
	task := &store.Task{
		ID: store.NewID(), RunID: "paused-task-run", ParentSessionID: store.NewID(),
		ChildSessionID: child.ID, Label: "audit", Status: protocol.TaskWorking,
	}
	if err := tasks.Create(ctx, task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	runner := NewRunner(ctx, db, &AgentDeps{
		AgentConfigs:     agentConfigs,
		Providers:        store.NewProviderStore(db),
		Sessions:         sessions,
		Settings:         settings.NewReader(store.NewSettingStore(db)),
		Memories:         store.NewMemoryStore(db),
		PendingApprovals: approvals,
		Tasks:            tasks,
	})

	var rawCall agents.OutputItem
	if err := json.Unmarshal([]byte(`{"type":"function_call","call_id":"call-race-1","name":"shell","arguments":"{}"}`), &rawCall); err != nil {
		t.Fatalf("unmarshal raw call: %v", err)
	}
	state := &agents.RunState{
		CurrentAgent: &agents.Agent{Name: "approver"},
		Approvals:    agents.NewApprovalStore(),
		Interruptions: []*agents.ToolApprovalItem{{
			Agent:    &agents.Agent{Name: "approver"},
			ToolName: "shell",
			CallID:   "call-race-1",
			Raw:      rawCall,
		}},
	}
	stateJSON, err := state.MarshalJSON()
	if err != nil {
		t.Fatalf("marshal run state: %v", err)
	}
	calls, _ := json.Marshal([]store.PendingToolCall{{ToolCallID: "call-race-1", ToolName: "shell"}})
	if err := approvals.Save(ctx, &store.PendingApproval{
		RunID:         task.RunID,
		SessionID:     child.ID,
		AgentConfigID: ac.ID,
		State:         string(stateJSON),
		ToolCalls:     calls,
	}); err != nil {
		t.Fatalf("save pending: %v", err)
	}

	_, _, err = runner.ResolveApproval(ctx, "call-race-1", true, ApprovalOnce, "", nil)
	if _, ok := errors.AsType[*ApprovalNotReadyError](err); !ok {
		t.Fatalf("want *ApprovalNotReadyError (retryable), got %v", err)
	}
	// The pending row MUST survive so the decision can be retried once postRun
	// marks the task input_required — losing it would strand the paused run.
	if _, err := approvals.Get(ctx, task.RunID); err != nil {
		t.Fatalf("pending row must survive a not-ready claim, got %v", err)
	}
	// The task row is untouched (still working, not spuriously terminal).
	if got, _ := tasks.Get(ctx, task.ID); got.Status != protocol.TaskWorking {
		t.Fatalf("task status = %q, want still working", got.Status)
	}

	// Once postRun marks it input_required, the same decision goes through.
	if err := tasks.MarkInputRequired(ctx, task.ID, task.RunID); err != nil {
		t.Fatalf("mark input_required: %v", err)
	}
	if _, _, err := runner.ResolveApproval(ctx, "call-race-1", true, ApprovalOnce, "", nil); err != nil {
		t.Fatalf("resolve after input_required: %v", err)
	}
	if _, err := approvals.Get(ctx, task.RunID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("pending row should be consumed after a successful resolve, got %v", err)
	}
}
