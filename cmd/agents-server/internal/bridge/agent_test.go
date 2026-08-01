package bridge

import (
	"context"
	"testing"

	"github.com/zzir/agents-go/agents/middleware"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// use_previous_response_id was removed end to end (the server always runs
// with a persisted session, which the SDK refuses to combine with
// previous-response chaining — the field spent its life stored, surfaced and
// then rejected). A legacy row whose session JSON still carries the key must
// simply decode past it and build.
func TestBuildFullAgentIgnoresLegacyUsePreviousResponseID(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	agentConfigs := store.NewAgentConfigStore(db)

	ac := &store.AgentConfig{Name: "legacy", Model: "gpt-test"}
	if err := agentConfigs.Create(ctx, ac); err != nil {
		t.Fatalf("create agent config: %v", err)
	}
	// Simulate the pre-removal row shape: the stale key inside the session
	// JSON column.
	if _, err := db.ExecContext(ctx,
		`UPDATE agent_configs SET session = ? WHERE id = ?`,
		`{"use_previous_response_id":true,"history_limit":5}`, ac.ID); err != nil {
		t.Fatalf("plant legacy session JSON: %v", err)
	}

	deps := &AgentDeps{
		AgentConfigs: agentConfigs,
		Settings:     store.NewSettingStore(db),
		Memories:     store.NewMemoryStore(db),
	}
	built, err := BuildFullAgent(ctx, deps, ac.ID, "")
	if err != nil {
		t.Fatalf("a legacy row with the stale key must build: %v", err)
	}
	// The keys that survived the removal still decode.
	if built.HistoryLimit != 5 {
		t.Errorf("history_limit = %d, want 5 — the rest of the session group must still load", built.HistoryLimit)
	}
}

// behavior.plan_mode / todo_list rewrite the built ENTRY agent at build time —
// the registry a resume rebuilds must carry submit_plan/todo_write, or the
// approved call fails with "tool not found on agent".
func TestBuildFullAgentAppliesWorkflowModes(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	agentConfigs := store.NewAgentConfigStore(db)

	ac := &store.AgentConfig{Name: "wf", Model: "gpt-test",
		Behavior: store.BehaviorGroup{PlanMode: true, TodoList: true}}
	if err := agentConfigs.Create(ctx, ac); err != nil {
		t.Fatalf("create agent config: %v", err)
	}
	deps := &AgentDeps{
		AgentConfigs: agentConfigs,
		Settings:     store.NewSettingStore(db),
		Memories:     store.NewMemoryStore(db),
	}
	built, err := BuildFullAgent(ctx, deps, ac.ID, "")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	names := map[string]bool{}
	for _, tl := range built.Agent.Tools {
		names[tl.ToolName()] = true
	}
	if !names[middleware.PlanToolName] || !names[middleware.TodoToolName] {
		t.Fatalf("workflow tools missing from build: %v", names)
	}
	if built.PlanPhase == nil {
		t.Fatal("plan mode build must expose its PlanPhase for the resume unlock")
	}
	if built.PlanPhase.Executing() {
		t.Fatal("a fresh build must start in the planning phase")
	}
}
