package bridge

import (
	"context"
	"strings"
	"testing"

	"github.com/zzir/agents-go/agents/middleware"
	"github.com/zzir/agents-go/cmd/agents-server/internal/settings"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/cmd/agents-server/internal/testdb"
)

// use_previous_response_id was removed end to end (the server always runs
// with a persisted session, which the SDK refuses to combine with
// previous-response chaining — the field spent its life stored, surfaced and
// then rejected). A legacy row whose session JSON still carries the key must
// simply decode past it and build.
func TestBuildFullAgentIgnoresLegacyUsePreviousResponseID(t *testing.T) {
	ctx := context.Background()
	db := testdb.New(t)
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
		Providers:    store.NewProviderStore(db),
		Settings:     settings.NewReader(store.NewSettingStore(db)),
		Memories:     store.NewMemoryStore(db),
	}
	built, err := BuildFullAgent(ctx, deps, ac.ID, "", store.LocalUserID)
	if err != nil {
		t.Fatalf("a legacy row with the stale key must build: %v", err)
	}
	// The keys that survived the removal still decode.
	if built.Session.HistoryLimit != 5 {
		t.Errorf("history_limit = %d, want 5 — the rest of the session group must still load", built.Session.HistoryLimit)
	}
}

// A background run is TOLD it is one. Removing its tools stops it doing the
// wrong things; it does not stop it ending a turn with a question, which in a
// session nobody reads is a deliverable nobody can answer.
func TestBackgroundBuildIsToldNobodyIsReading(t *testing.T) {
	ctx := context.Background()
	db := testdb.New(t)
	agentConfigs := store.NewAgentConfigStore(db)
	ac := &store.AgentConfig{Name: "worker", Model: "gpt-test", Instructions: "Be helpful."}
	if err := agentConfigs.Create(ctx, ac); err != nil {
		t.Fatal(err)
	}
	deps := &AgentDeps{
		AgentConfigs: agentConfigs,
		Providers:    store.NewProviderStore(db),
		Settings:     settings.NewReader(store.NewSettingStore(db)),
		Memories:     store.NewMemoryStore(db),
	}
	instructionsOf := func(background bool) string {
		built, err := buildFullAgent(ctx, deps, ac.ID, "", "", background, "")
		if err != nil {
			t.Fatalf("build (background=%v): %v", background, err)
		}
		text, err := built.Agent.Instructions(ctx, nil, built.Agent)
		if err != nil {
			t.Fatalf("instructions (background=%v): %v", background, err)
		}
		return text
	}
	bg := instructionsOf(true)
	if !strings.Contains(bg, BackgroundInstructions) {
		t.Fatalf("a background build must carry the preamble; got:\n%s", bg)
	}
	if !strings.Contains(bg, "Be helpful.") {
		t.Error("the agent's own instructions must survive")
	}
	// And it stays off a chat run, which does have somebody to ask.
	if chat := instructionsOf(false); strings.Contains(chat, BackgroundInstructions) {
		t.Errorf("a chat build must not be told nobody is reading; got:\n%s", chat)
	}
}

// Plan and Todo rewrite the built ENTRY agent at build time, for every chat
// agent — the registry a resume rebuilds must carry submit_plan/todo_write, or
// the approved call fails with "tool not found on agent".
func TestBuildFullAgentAppliesWorkflowModes(t *testing.T) {
	ctx := context.Background()
	db := testdb.New(t)
	agentConfigs := store.NewAgentConfigStore(db)

	ac := &store.AgentConfig{Name: "wf", Model: "gpt-test"}
	if err := agentConfigs.Create(ctx, ac); err != nil {
		t.Fatalf("create agent config: %v", err)
	}
	deps := &AgentDeps{
		AgentConfigs: agentConfigs,
		Providers:    store.NewProviderStore(db),
		Settings:     settings.NewReader(store.NewSettingStore(db)),
		Memories:     store.NewMemoryStore(db),
	}
	built, err := BuildFullAgent(ctx, deps, ac.ID, "", store.LocalUserID)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	names := map[string]bool{}
	for _, tl := range built.Agent.Tools {
		names[tl.Name] = true
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
