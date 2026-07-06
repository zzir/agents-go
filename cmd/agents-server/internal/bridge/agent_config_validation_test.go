package bridge

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// Critical config that fails to parse/resolve must fail the build loudly, not
// silently no-op: a guardrail or output schema that "looks enabled" but never
// runs is the dangerous case. A malformed skills selection must fail rather
// than fall open to the full skill set.
func TestBuildFullAgentFailsOnBadCriticalConfig(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	s := store.NewAgentConfigStore(db)
	// A workspace with an (empty) skills dir so the skills block is exercised.
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "skills"), 0o755); err != nil {
		t.Fatalf("mkdir skills: %v", err)
	}
	deps := &AgentDeps{
		AgentConfigs: s,
		Settings:     store.NewSettingStore(db),
		Memories:     store.NewMemoryStore(db),
		Guardrails:   NewGuardrailResolver(store.NewGuardrailStore(db)),
		Workspace:    ws,
	}

	cases := []struct {
		name    string
		mutate  func(*store.AgentConfig)
		wantSub string
	}{
		{"bad output_schema", func(a *store.AgentConfig) { a.OutputSchema = "{not json" }, "output_schema"},
		{"unknown input guardrail", func(a *store.AgentConfig) { a.InputGuardrails = `["no_such_guardrail"]` }, "not found"},
		{"unknown output guardrail", func(a *store.AgentConfig) { a.OutputGuardrails = `["nope"]` }, "not found"},
		{"bad approve_tools", func(a *store.AgentConfig) { a.ApproveTools = "[not json" }, "approve_tools"},
		{"wrong-typed model_settings", func(a *store.AgentConfig) { a.ModelSettings = `{"temperature":"hot"}` }, "model_settings"},
		{"malformed skills selection", func(a *store.AgentConfig) { a.SkillsJSON = "[not json" }, "skills"},
		{"malformed handoffs", func(a *store.AgentConfig) { a.HandoffsJSON = "{bad" }, "handoffs"},
		{"malformed tools", func(a *store.AgentConfig) { a.ToolsJSON = "not-json" }, "tools"},
		{"malformed retry_policy", func(a *store.AgentConfig) { a.APIKey = "sk-x"; a.RetryEnabled = true; a.RetryPolicy = "{bad" }, "retry_policy"},
		{"malformed fallback_models", func(a *store.AgentConfig) { a.APIKey = "sk-x"; a.FallbackModels = "{bad" }, "fallback_models"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ac := &store.AgentConfig{Name: "a-" + tc.name, Model: "gpt-test"}
			tc.mutate(ac)
			if err := s.Create(ctx, ac); err != nil {
				t.Fatalf("create: %v", err)
			}
			_, err := BuildFullAgent(ctx, deps, ac.ID, "")
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("want error containing %q, got %v", tc.wantSub, err)
			}
		})
	}

	// A built-in guardrail name resolves and builds fine.
	ok := &store.AgentConfig{Name: "ok", Model: "gpt-test", InputGuardrails: `["content_filter"]`}
	if err := s.Create(ctx, ok); err != nil {
		t.Fatalf("create ok: %v", err)
	}
	if _, err := BuildFullAgent(ctx, deps, ok.ID, ""); err != nil {
		t.Fatalf("built-in guardrail should build: %v", err)
	}
}
