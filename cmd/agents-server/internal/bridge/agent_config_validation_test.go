package bridge

import (
	"context"
	"strings"
	"testing"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/cmd/agents-server/internal/guardrails"
	"github.com/zzir/agents-go/cmd/agents-server/internal/settings"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/cmd/agents-server/internal/testdb"
)

// Critical config that fails to parse/resolve must fail the build loudly, not
// silently no-op: a guardrail or output schema that "looks enabled" but never
// runs is the dangerous case. A malformed skills selection must fail rather
// than fall open to the full skill set.
func TestBuildFullAgentFailsOnBadCriticalConfig(t *testing.T) {
	ctx := context.Background()
	db := testdb.New(t)
	s := store.NewAgentConfigStore(db)
	deps := &AgentDeps{
		AgentConfigs: s,
		Settings:     settings.NewReader(store.NewSettingStore(db)),
		Skills:       store.NewSkillStore(db),
		Memories:     store.NewMemoryStore(db),
		Guardrails:   guardrails.NewResolver(store.NewGuardrailStore(db)),
		Workspace:    t.TempDir(),
	}

	cases := []struct {
		name    string
		mutate  func(*store.AgentConfig)
		wantSub string
	}{
		{"bad output_schema", func(a *store.AgentConfig) { a.Guardrails.OutputSchema = "{not json" }, "output_schema"},
		{"unknown guardrail", func(a *store.AgentConfig) { a.Guardrails.Guardrails = `["no_such_guardrail"]` }, "not found"},
		{"bad approve_tools", func(a *store.AgentConfig) { a.Approval.ApproveTools = "[not json" }, "approve_tools"},
		{"wrong-typed model_settings", func(a *store.AgentConfig) { a.ModelSettings = `{"temperature":"hot"}` }, "model_settings"},
		{"malformed skills selection", func(a *store.AgentConfig) { a.SkillsJSON = "[not json" }, "skills"},
		{"malformed handoffs", func(a *store.AgentConfig) { a.HandoffsJSON = "{bad" }, "handoffs"},
		{"malformed tools", func(a *store.AgentConfig) { a.ToolsJSON = "not-json" }, "tools"},
		{"malformed retry_policy", func(a *store.AgentConfig) {
			a.Resilience.RetryEnabled = true
			a.Resilience.RetryPolicy = "{bad"
		}, "retry_policy"},
		{"malformed fallback_models", func(a *store.AgentConfig) {
			a.Resilience.FallbackModels = "{bad"
		}, "fallback_models"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ac := &store.AgentConfig{Name: "a-" + tc.name, Model: "gpt-test"}
			tc.mutate(ac)
			if err := s.Create(ctx, ac); err != nil {
				t.Fatalf("create: %v", err)
			}
			_, err := BuildFullAgent(ctx, deps, ac.ID, "", store.LocalUserID)
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("want error containing %q, got %v", tc.wantSub, err)
			}
		})
	}

	// A built-in guardrail name resolves and builds fine.
	ok := &store.AgentConfig{Name: "ok", Model: "gpt-test", Guardrails: store.GuardrailGroup{Guardrails: `["content_filter"]`}}
	if err := s.Create(ctx, ok); err != nil {
		t.Fatalf("create ok: %v", err)
	}
	if _, err := BuildFullAgent(ctx, deps, ok.ID, "", store.LocalUserID); err != nil {
		t.Fatalf("built-in guardrail should build: %v", err)
	}
}

// The entry agent's guardrails are lifted to the run level (and cleared off the
// agent) so they protect the whole run — including the final output after a
// handoff to an agent with no guardrails of its own.
func TestBuildFullAgentPromotesGuardrailsToRunLevel(t *testing.T) {
	ctx := context.Background()
	db := testdb.New(t)
	s := store.NewAgentConfigStore(db)
	deps := &AgentDeps{
		AgentConfigs: s,
		Settings:     settings.NewReader(store.NewSettingStore(db)),
		Memories:     store.NewMemoryStore(db),
		Guardrails:   guardrails.NewResolver(store.NewGuardrailStore(db)),
	}
	ac := &store.AgentConfig{
		Name:  "guarded",
		Model: "gpt-test",
		Guardrails: store.GuardrailGroup{
			Guardrails: `["content_filter", "max_output_length"]`,
		},
	}
	if err := s.Create(ctx, ac); err != nil {
		t.Fatal(err)
	}
	built, err := BuildFullAgent(ctx, deps, ac.ID, "", store.LocalUserID)
	if err != nil {
		t.Fatal(err)
	}
	if len(built.Agent.Guardrails) != 0 {
		t.Errorf("root agent still carries %d guardrail(s); they should be lifted to the run level",
			len(built.Agent.Guardrails))
	}
	if len(built.RunGuardrails) != 2 {
		t.Errorf("RunGuardrails = %d, want 2 (one input + one output)", len(built.RunGuardrails))
	}
	var sawInput, sawOutput bool
	for _, g := range built.RunGuardrails {
		sawInput = sawInput || g.Covers(agents.StageInput)
		sawOutput = sawOutput || g.Covers(agents.StageOutput)
	}
	if !sawInput || !sawOutput {
		t.Errorf("RunGuardrails stages: input=%v output=%v, want both", sawInput, sawOutput)
	}
}
