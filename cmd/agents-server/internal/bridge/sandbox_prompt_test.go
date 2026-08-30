package bridge

import (
	"context"
	"strings"
	"testing"

	"github.com/zzir/agents-go/cmd/agents-server/internal/sandboxes"
	"github.com/zzir/agents-go/cmd/agents-server/internal/settings"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/cmd/agents-server/internal/testdb"
	"github.com/zzir/agents-go/sandbox"
)

// A sandbox carries a prompt describing the machine; a session bound to a
// project on it gets that prompt appended to its agent's instructions — after
// the agent's own, and measured into the context profile.
func TestBuildFullAgentAppendsSandboxPrompt(t *testing.T) {
	ctx := context.Background()
	db := testdb.New(t)
	agentConfigs := store.NewAgentConfigStore(db)
	sandboxStore := store.NewSandboxStore(db)
	projects := store.NewProjectStore(db)

	ac := &store.AgentConfig{OwnerID: store.LocalUserID, Name: "coder", Model: "gpt-test", Instructions: "Be precise."}
	if err := agentConfigs.Create(ctx, ac); err != nil {
		t.Fatal(err)
	}
	const prompt = "This machine runs Alpine; use apk. No outbound network."
	sb := &store.Sandbox{ID: store.NewID(), Name: "box", Type: "docker", Config: []byte(`{"image":"img"}`), Prompt: prompt}
	if err := sandboxStore.Create(ctx, sb); err != nil {
		t.Fatal(err)
	}
	proj := &store.Project{ID: store.NewID(), OwnerID: store.LocalUserID, SandboxID: sb.ID, Name: "p"}
	if err := projects.Create(ctx, proj); err != nil {
		t.Fatal(err)
	}

	mgr := sandboxes.NewManager()
	mgr.SetBuildOverride(func(sandboxes.Spec) (sandbox.Sandbox, error) {
		return sandbox.NewLocalWithOptions(sandbox.LocalOptions{WorkDir: t.TempDir()}), nil
	})
	deps := &AgentDeps{
		AgentConfigs:   agentConfigs,
		Providers:      store.NewProviderStore(db),
		Settings:       settings.NewReader(store.NewSettingStore(db)),
		Memories:       store.NewMemoryStore(db),
		Sandboxes:      sandboxStore,
		Projects:       projects,
		SandboxManager: mgr,
	}

	built, err := BuildFullAgent(ctx, deps, ac.ID, proj.ID, store.LocalUserID)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	defer built.Release()

	text, err := built.Agent.Instructions(ctx, nil, built.Agent)
	if err != nil {
		t.Fatalf("instructions: %v", err)
	}
	own, at := strings.Index(text, "Be precise."), strings.Index(text, prompt)
	if at < 0 {
		t.Fatalf("built instructions must carry the sandbox prompt; got:\n%s", text)
	}
	if own < 0 || at < own {
		t.Errorf("the sandbox prompt must follow the agent's own instructions (own=%d, sandbox=%d)", own, at)
	}
	if built.Profile.SandboxPromptChars != len(prompt) {
		t.Errorf("profile sandbox_prompt_chars = %d, want %d", built.Profile.SandboxPromptChars, len(prompt))
	}
}

// An empty sandbox prompt injects nothing and measures zero — no phantom
// instruction layer.
func TestBuildFullAgentNoSandboxPromptWhenEmpty(t *testing.T) {
	ctx := context.Background()
	db := testdb.New(t)
	agentConfigs := store.NewAgentConfigStore(db)
	sandboxStore := store.NewSandboxStore(db)
	projects := store.NewProjectStore(db)

	ac := &store.AgentConfig{OwnerID: store.LocalUserID, Name: "coder", Model: "gpt-test", Instructions: "Be precise."}
	if err := agentConfigs.Create(ctx, ac); err != nil {
		t.Fatal(err)
	}
	sb := &store.Sandbox{ID: store.NewID(), Name: "box", Type: "docker", Config: []byte(`{"image":"img"}`)}
	if err := sandboxStore.Create(ctx, sb); err != nil {
		t.Fatal(err)
	}
	proj := &store.Project{ID: store.NewID(), OwnerID: store.LocalUserID, SandboxID: sb.ID, Name: "p"}
	if err := projects.Create(ctx, proj); err != nil {
		t.Fatal(err)
	}

	mgr := sandboxes.NewManager()
	mgr.SetBuildOverride(func(sandboxes.Spec) (sandbox.Sandbox, error) {
		return sandbox.NewLocalWithOptions(sandbox.LocalOptions{WorkDir: t.TempDir()}), nil
	})
	deps := &AgentDeps{
		AgentConfigs:   agentConfigs,
		Providers:      store.NewProviderStore(db),
		Settings:       settings.NewReader(store.NewSettingStore(db)),
		Memories:       store.NewMemoryStore(db),
		Sandboxes:      sandboxStore,
		Projects:       projects,
		SandboxManager: mgr,
	}

	built, err := BuildFullAgent(ctx, deps, ac.ID, proj.ID, store.LocalUserID)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	defer built.Release()

	if built.Profile.SandboxPromptChars != 0 {
		t.Errorf("profile sandbox_prompt_chars = %d, want 0 for an empty prompt", built.Profile.SandboxPromptChars)
	}
	text, err := built.Agent.Instructions(ctx, nil, built.Agent)
	if err != nil {
		t.Fatalf("instructions: %v", err)
	}
	if !strings.Contains(text, "Be precise.") {
		t.Errorf("the agent's own instructions must survive; got:\n%s", text)
	}
}
