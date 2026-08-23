package bridge

import (
	"context"
	"testing"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/cmd/agents-server/internal/sandboxes"
	"github.com/zzir/agents-go/cmd/agents-server/internal/settings"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/cmd/agents-server/internal/testdb"
)

// buildAgentRegistry resolves the names in a serialized RunState back to live
// agents, and that resolved agent is the one the SDK re-runs on approval. So
// the registry MUST carry the run's sandbox-backed tools; building it with an
// empty sandbox id strips exec_command/read_file/… and the approved call fails
// with "tool not found on agent" (regression: an approval-gated sandbox tool
// could never be approved).
func TestBuildAgentRegistryIncludesSandboxTools(t *testing.T) {
	ctx := context.Background()
	db := testdb.New(t)

	agentConfigs := store.NewAgentConfigStore(db)
	sandboxStore := store.NewSandboxStore(db)

	ac := &store.AgentConfig{Name: "coder", Model: "gpt-test", ProviderID: testProvider(t, db, "p", "sk-x", "")}
	if err := agentConfigs.Create(ctx, ac); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	// A local sandbox needs no daemon, so its tools build in-process.
	sb := &store.SandboxConfig{ID: store.NewID(), Name: "L", Type: "local", Config: []byte(`{}`)}
	if err := sandboxStore.Create(ctx, sb); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	runner := NewRunner(ctx, db, &AgentDeps{
		AgentConfigs:   agentConfigs,
		Providers:      store.NewProviderStore(db),
		SandboxConfigs: sandboxStore,
		Settings:       settings.NewReader(store.NewSettingStore(db)),
		Memories:       store.NewMemoryStore(db),
		McpServers:     store.NewMcpServerStore(db),
		ProviderRoutes: store.NewProviderRouteStore(db),
		Guardrails:     NewGuardrailResolver(store.NewGuardrailStore(db)),
		McpManager:     NewMcpManager(ctx, settings.NewReader(store.NewSettingStore(db))),
		SandboxManager: sandboxes.NewManager(t.TempDir()),
	})

	hasExec := func(reg map[string]*agents.Agent) bool {
		a := reg["coder"]
		if a == nil {
			t.Fatal("registry missing the agent")
		}
		for _, tool := range a.Tools {
			if tool.Name == "exec_command" {
				return true
			}
		}
		return false
	}

	// With the sandbox id: the resolved agent must carry exec_command.
	withSb, _, err := runner.buildAgentRegistry(ctx, ac.ID, sb.ID, "", false, "")
	if err != nil {
		t.Fatalf("buildAgentRegistry(sandbox, false): %v", err)
	}
	if !hasExec(withSb) {
		t.Error("registry built with a sandbox id is missing exec_command")
	}

	// Without it, exec_command is absent — this is exactly the state that
	// stranded approvals, so the fix is that ResolveApproval passes the id.
	noSb, _, err := runner.buildAgentRegistry(ctx, ac.ID, "", "", false, "")
	if err != nil {
		t.Fatalf("buildAgentRegistry(none, false): %v", err)
	}
	if hasExec(noSb) {
		t.Error("registry built with no sandbox id unexpectedly has exec_command")
	}
}
