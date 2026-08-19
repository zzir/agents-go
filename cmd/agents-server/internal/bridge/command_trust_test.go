package bridge

import (
	"context"
	"testing"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/cmd/agents-server/internal/settings"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// commandHash is exact over (cmd, workdir): identical args hash equal, any
// change hashes differently (so "approve this command" can't be widened).
func TestCommandHash(t *testing.T) {
	h1 := commandHash(`{"cmd":"go test","workdir":"a"}`)
	h2 := commandHash(`{"cmd":"go test","workdir":"a"}`)
	h3 := commandHash(`{"cmd":"go test","workdir":"b"}`)
	h4 := commandHash(`{"cmd":"go test ./...","workdir":"a"}`)
	if h1 != h2 {
		t.Fatal("identical args must hash equal")
	}
	if h1 == h3 || h1 == h4 {
		t.Fatal("different cmd/workdir must hash differently")
	}
}

// commandGate requires approval unless the session has trusted this exact
// command or all commands.
func TestCommandGate(t *testing.T) {
	m := NewSandboxManager(t.TempDir())
	args := `{"cmd":"ls","workdir":""}`
	rc := &agents.RunContext{Context: "sess1"}

	// nil rc / no session context → require approval (fail safe).
	if need, _ := m.commandGate(context.Background(), nil, args, ""); !need {
		t.Fatal("nil rc → should require approval")
	}
	if need, _ := m.commandGate(context.Background(), &agents.RunContext{}, args, ""); !need {
		t.Fatal("no session → should require approval")
	}
	// Fresh session, untrusted → require approval.
	if need, _ := m.commandGate(context.Background(), rc, args, ""); !need {
		t.Fatal("untrusted → should require approval")
	}
	// Trust this exact command → no approval; a different command still requires it.
	m.Trust().forSession("sess1").allowCommand(commandHash(args))
	if need, _ := m.commandGate(context.Background(), rc, args, ""); need {
		t.Fatal("trusted command → should NOT require approval")
	}
	if need, _ := m.commandGate(context.Background(), rc, `{"cmd":"rm -rf x"}`, ""); !need {
		t.Fatal("different command in trusted session → should still require approval")
	}
	// allowAll → nothing in that session requires approval.
	m.Trust().forSession("sess2").allowAll()
	rc2 := &agents.RunContext{Context: "sess2"}
	if need, _ := m.commandGate(context.Background(), rc2, `{"cmd":"anything"}`, ""); need {
		t.Fatal("approveAll → should NOT require approval")
	}
}

// "exec_command" in approve_tools opts into the session command gate, so it must
// be stripped from the SDK ApproveTools list (which would otherwise force
// approval on every call and defeat session trust); other tool names survive.
func TestExecCommandRoutedOffSDKApproveList(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
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
	built, err := buildFullAgent(ctx, deps, ac.ID, "", "", true)
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
	fg, err := BuildFullAgent(ctx, deps, ac.ID, "")
	if err != nil {
		t.Fatalf("foreground build: %v", err)
	}
	if len(fg.Agent.ApproveTools) != 0 {
		t.Fatalf("foreground ApproveTools = %v, want none (translated by the plan gate)", fg.Agent.ApproveTools)
	}
}
