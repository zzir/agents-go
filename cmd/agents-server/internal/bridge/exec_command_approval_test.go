package bridge

import (
	"context"
	"testing"

	"github.com/zzir/agents-go/cmd/agents-server/internal/settings"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/cmd/agents-server/internal/testdb"
)

// "exec_command" in approve_tools opts into the session command gate, so it must
// be stripped from the SDK ApproveTools list (which would otherwise force
// approval on every call and defeat session trust); other tool names survive.
func TestExecCommandRoutedOffSDKApproveList(t *testing.T) {
	ctx := context.Background()
	db := testdb.New(t)
	s := store.NewAgentConfigStore(db)
	deps := &AgentDeps{
		AgentConfigs: s,
		Settings:     settings.NewReader(store.NewSettingStore(db)),
		Memories:     store.NewMemoryStore(db),
		Workspace:    t.TempDir(),
	}
	ac := &store.AgentConfig{Name: "a", Model: "gpt-test", Approval: store.ApprovalGroup{ApproveTools: `["exec_command","other_tool"]`}}
	if err := s.Create(ctx, ac); err != nil {
		t.Fatalf("create: %v", err)
	}
	// A background build keeps the raw list (no plan gate), which is where the
	// routing filter is observable.
	built, err := buildFullAgent(ctx, deps, ac.ID, "", "", true, "")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	for _, n := range built.Agent.ApproveTools {
		if n == "exec_command" {
			t.Fatalf("exec_command must be stripped from SDK ApproveTools, got %v", built.Agent.ApproveTools)
		}
	}
	if len(built.Agent.ApproveTools) != 1 || built.Agent.ApproveTools[0] != "other_tool" {
		t.Fatalf("ApproveTools = %v, want [other_tool]", built.Agent.ApproveTools)
	}

	// A foreground build hands the (filtered) list to the plan gate, which
	// translates it into per-tool predicates and clears it — the phase must be
	// able to suppress approval on a call it is refusing anyway.
	fg, err := BuildFullAgent(ctx, deps, ac.ID, "", store.LocalUserID)
	if err != nil {
		t.Fatalf("foreground build: %v", err)
	}
	if len(fg.Agent.ApproveTools) != 0 {
		t.Fatalf("foreground ApproveTools = %v, want none (translated by the plan gate)", fg.Agent.ApproveTools)
	}
}
