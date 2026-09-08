package bridge

import (
	"context"
	"strings"
	"testing"

	"github.com/zzir/agents-go/agents"
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

	ac := &store.AgentConfig{OwnerID: store.LocalUserID, Name: "legacy", Model: "gpt-test"}
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
	ac := &store.AgentConfig{OwnerID: store.LocalUserID, Name: "worker", Model: "gpt-test", Instructions: "Be helpful."}
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
		built, err := buildFullAgent(ctx, deps, ac.ID, "", background, "")
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

	ac := &store.AgentConfig{OwnerID: store.LocalUserID, Name: "wf", Model: "gpt-test"}
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

// Run-time provider resolution re-checks the reference rule: a demote that
// slipped past the write-time guards must fail the build loudly, never spend
// a key that became somebody's private credential (decisions §5.29).
func TestAgentProviderRechecksScope(t *testing.T) {
	ctx := context.Background()
	db := testdb.New(t)
	providers := store.NewProviderStore(db)
	pv := &store.Provider{OwnerID: store.LocalUserID, Name: "shared", Type: "openai", APIKey: "sk-x", Scope: store.ScopeGlobal}
	if err := providers.Create(ctx, pv); err != nil {
		t.Fatal(err)
	}
	agentConfigs := store.NewAgentConfigStore(db)
	ac := &store.AgentConfig{OwnerID: store.LocalUserID, Name: "g", Model: "m", ProviderID: pv.ID, Scope: store.ScopeGlobal}
	if err := agentConfigs.Create(ctx, ac); err != nil {
		t.Fatal(err)
	}
	// Slip a demote past the guards by writing the columns directly.
	if _, err := db.ExecContext(ctx, `UPDATE providers SET scope = 'private', owner_id = ? WHERE id = ?`,
		store.NewID(), pv.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := AgentProvider(ctx, &AgentDeps{Providers: providers}, ac); err == nil || !strings.Contains(err.Error(), "scope") {
		t.Fatalf("global agent on a demoted provider = %v, want a scope refusal", err)
	}
}

// The system_prompt setting wraps every agent's instructions, unless the
// agent overrides it: then its own text goes alone, empty included
// (invariant 67).
func TestBuildFullAgentOverridesSystemPrompt(t *testing.T) {
	ctx := context.Background()
	db := testdb.New(t)
	agentConfigs := store.NewAgentConfigStore(db)
	settingStore := store.NewSettingStore(db)
	if err := settingStore.Set(ctx, settings.KeySystemPrompt, "Global rules."); err != nil {
		t.Fatal(err)
	}
	deps := &AgentDeps{
		AgentConfigs: agentConfigs,
		Providers:    store.NewProviderStore(db),
		Settings:     settings.NewReader(settingStore),
		Memories:     store.NewMemoryStore(db),
	}
	resolve := func(a *agents.Agent) string {
		t.Helper()
		if a.Instructions == nil {
			return ""
		}
		text, err := a.Instructions(ctx, nil, a)
		if err != nil {
			t.Fatal(err)
		}
		return text
	}
	override := store.BehaviorGroup{OverrideSystemPrompt: true}

	// The layer itself: what the model gets as its system prompt.
	layer := func(ac *store.AgentConfig) (string, store.PromptProfile) {
		agent := &agents.Agent{Name: ac.Name}
		if ac.Instructions != "" {
			agent.Instructions = agents.StaticInstructions(ac.Instructions)
		}
		var prof store.PromptProfile
		layerInstructions(ctx, deps, agent, ac, &prof)
		return resolve(agent), prof
	}
	if text, prof := layer(&store.AgentConfig{Name: "inherits", Instructions: "Be brief."}); text != "Global rules.\n\nBe brief." || prof.GlobalPromptChars != len("Global rules.") {
		t.Errorf("without the override the global prompt wraps the agent's own and is measured; got %q, %d chars", text, prof.GlobalPromptChars)
	}
	if text, prof := layer(&store.AgentConfig{Name: "overrides", Instructions: "Be brief.", Behavior: override}); text != "Be brief." || prof.GlobalPromptChars != 0 {
		t.Errorf("with the override the agent's own text goes alone; got %q, %d chars", text, prof.GlobalPromptChars)
	}
	if text, prof := layer(&store.AgentConfig{Name: "empty", Behavior: override}); text != "" || prof.GlobalPromptChars != 0 {
		t.Errorf("an overriding agent with no text sends no system prompt at all; got %q, %d chars", text, prof.GlobalPromptChars)
	}

	// End to end, through the stored row and the full chat build (which wraps
	// the text in its own guidance).
	build := func(ac *store.AgentConfig) (string, *BuildResult) {
		t.Helper()
		if err := agentConfigs.Create(ctx, ac); err != nil {
			t.Fatal(err)
		}
		built, err := BuildFullAgent(ctx, deps, ac.ID, "", store.LocalUserID)
		if err != nil {
			t.Fatalf("build %q: %v", ac.Name, err)
		}
		return resolve(built.Agent), built
	}
	if text, built := build(&store.AgentConfig{OwnerID: store.LocalUserID, Name: "inherits", Model: "gpt-test", Instructions: "Be brief."}); !strings.Contains(text, "Global rules.\n\nBe brief.") || built.Profile.GlobalPromptChars != len("Global rules.") {
		t.Errorf("a stored agent without the override carries the global prompt before its own text; got:\n%s", text)
	}
	if text, built := build(&store.AgentConfig{OwnerID: store.LocalUserID, Name: "overrides", Model: "gpt-test", Instructions: "Be brief.", Behavior: override}); !strings.Contains(text, "Be brief.") || strings.Contains(text, "Global rules.") || built.Profile.GlobalPromptChars != 0 {
		t.Errorf("a stored agent with the override carries its own text and no global prompt; got:\n%s", text)
	}
}
