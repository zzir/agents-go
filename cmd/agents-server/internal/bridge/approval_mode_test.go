package bridge

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/middleware"
	"github.com/zzir/agents-go/cmd/agents-server/internal/mcpservers"
	"github.com/zzir/agents-go/cmd/agents-server/internal/sandboxes"
	"github.com/zzir/agents-go/cmd/agents-server/internal/settings"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/cmd/agents-server/internal/testdb"
	"github.com/zzir/agents-go/sandbox"
)

// The mode is an enum refused at save, and "*" cannot ride beside a mode
// that asks: it would route exec_command around its per-command gate.
func TestDecodeAgentSpecApprovalMode(t *testing.T) {
	for _, mode := range []string{"", store.ApprovalModeNever, store.ApprovalModeOnChange, store.ApprovalModeAlways} {
		ac := &store.AgentConfig{Name: "a", Model: "m", Approval: store.ApprovalGroup{Mode: mode, ApproveTools: store.StringList{"write_file"}}}
		if _, err := DecodeAgentSpec(ac); err != nil {
			t.Fatalf("mode %q: %v", mode, err)
		}
	}
	if _, err := DecodeAgentSpec(&store.AgentConfig{Name: "a", Model: "m", Approval: store.ApprovalGroup{Mode: "sometimes"}}); err == nil || !strings.Contains(err.Error(), "approval mode") {
		t.Fatalf("an unknown mode: %v", err)
	}
	for _, mode := range []string{"", store.ApprovalModeNever} {
		if _, err := DecodeAgentSpec(&store.AgentConfig{Name: "a", Model: "m", Approval: store.ApprovalGroup{Mode: mode, ApproveTools: store.StringList{"*"}}}); err != nil {
			t.Fatalf("the legacy \"*\" under mode %q: %v", mode, err)
		}
	}
	if _, err := DecodeAgentSpec(&store.AgentConfig{Name: "a", Model: "m", Approval: store.ApprovalGroup{Mode: store.ApprovalModeOnChange, ApproveTools: store.StringList{"*"}}}); err == nil || !strings.Contains(err.Error(), `"*"`) {
		t.Fatalf("\"*\" beside on_change: %v", err)
	}
}

// asks reports what a tool's own predicate answers for one call (the agent
// list is not consulted: the mode must stand on the predicates alone).
func asks(t *testing.T, tool *agents.Tool, rc *agents.RunContext) bool {
	t.Helper()
	if tool.NeedsApprovalFunc == nil {
		return tool.NeedsApproval
	}
	need, err := tool.NeedsApprovalFunc(context.Background(), rc, `{"cmd":"true","timeout_seconds":0,"workdir":"","session_id":""}`, "c1")
	if err != nil {
		t.Fatalf("%s: predicate: %v", tool.Name, err)
	}
	return need
}

// deniedWhilePlanning reports whether a call while planning is refused.
func deniedWhilePlanning(t *testing.T, tool *agents.Tool) bool {
	t.Helper()
	out, err := tool.OnInvoke(context.Background(), &agents.ToolContext{}, `{}`)
	if err != nil {
		return false // a real tool fault, not the gate: the gate answers as text
	}
	s, _ := out.ModelOutput().(string)
	return strings.Contains(s, "disabled while planning")
}

// approvalModeRunner is a runner over a project with a local sandbox and an
// entry agent that hands off to a target, both in the given mode.
func approvalModeRunner(t *testing.T, mode string, approveTools ...string) (*Runner, *store.AgentConfig, *store.Project) {
	t.Helper()
	ctx := context.Background()
	db := testdb.New(t)
	agentConfigs := store.NewAgentConfigStore(db)
	targets := store.NewSandboxStore(db)
	tg := &store.Sandbox{ID: store.NewID(), Name: "L", Type: "docker", Config: []byte(`{"image":"i"}`)}
	if err := targets.Create(ctx, tg); err != nil {
		t.Fatal(err)
	}
	pid := testProvider(t, db, "p", "sk-x", "")
	approval := store.ApprovalGroup{Mode: mode, ApproveTools: approveTools}
	target := &store.AgentConfig{OwnerID: store.LocalUserID, Name: "target", Model: "gpt-test", ProviderID: pid, Approval: approval}
	if err := agentConfigs.Create(ctx, target); err != nil {
		t.Fatal(err)
	}
	entry := &store.AgentConfig{OwnerID: store.LocalUserID, Name: "entry", Model: "gpt-test", ProviderID: pid, Approval: approval,
		Behavior: store.BehaviorGroup{Checklist: true}, Handoffs: []string{target.ID}}
	if err := agentConfigs.Create(ctx, entry); err != nil {
		t.Fatal(err)
	}
	mgr := sandboxes.NewManager()
	mgr.SetBuildOverride(func(sandboxes.Spec) (sandbox.Sandbox, error) { return sandbox.NewLocal(), nil })
	runner := NewRunner(ctx, db, &AgentDeps{
		AgentConfigs:   agentConfigs,
		Providers:      store.NewProviderStore(db),
		Sandboxes:      targets,
		Projects:       store.NewProjectStore(db),
		Sessions:       store.NewSessionStore(db),
		Tasks:          store.NewTaskStore(db),
		Settings:       settings.NewReader(store.NewSettingStore(db)),
		Memories:       store.NewMemoryStore(db),
		McpManager:     mcpservers.NewManager(ctx, settings.NewReader(store.NewSettingStore(db))),
		SandboxManager: mgr,
		Traces:         store.NewTraceStore(db),
	})
	proj := &store.Project{OwnerID: store.LocalUserID, SandboxID: tg.ID, Name: "p"}
	if err := runner.Deps.Projects.Create(ctx, proj); err != nil {
		t.Fatal(err)
	}
	return runner, entry, proj
}

// on_change asks about exactly the tools plan mode refuses, tool by tool, on
// the entry agent (sandbox, tasks, checklist) and on a handoff target that
// plan mode never touches — invariant 90.
func TestApprovalModeOnChangeGatesExactlyThePlanDenySet(t *testing.T) {
	runner, entry, proj := approvalModeRunner(t, store.ApprovalModeOnChange)
	built, err := buildFullAgent(context.Background(), runner.Deps, entry.ID, proj.ID, false, store.LocalUserID)
	if err != nil {
		t.Fatal(err)
	}
	defer built.Release()
	compare := func(name string, agent *agents.Agent, phase *middleware.PlanPhase, wantAsked ...string) {
		t.Helper()
		denied := map[string]bool{}
		for _, tool := range agent.Tools {
			if tool.Name == middleware.PlanToolName {
				continue
			}
			denied[tool.Name] = deniedWhilePlanning(t, tool)
		}
		if err := phase.Unlock(); err != nil {
			t.Fatal(err)
		}
		var asked, refused []string
		for _, tool := range agent.Tools {
			if tool.Name == middleware.PlanToolName {
				continue
			}
			if asks(t, tool, nil) {
				asked = append(asked, tool.Name)
			}
			if denied[tool.Name] {
				refused = append(refused, tool.Name)
			}
		}
		slices.Sort(asked)
		slices.Sort(refused)
		if !slices.Equal(asked, refused) {
			t.Errorf("%s: on_change asks %v, plan mode refuses %v", name, asked, refused)
		}
		// The sets are what the surface promises: writes in, reads out.
		for _, want := range append([]string{"apply_patch", "write_file", execCommandToolName}, wantAsked...) {
			if !slices.Contains(asked, want) {
				t.Errorf("%s: %s is not asked about", name, want)
			}
		}
		for _, read := range []string{"read_file", "list_files", "task_status"} {
			if slices.Contains(asked, read) {
				t.Errorf("%s: %s is asked about", name, read)
			}
		}
	}
	compare("entry", built.Agent, built.PlanPhase, ChecklistToolName, SpawnToolName)

	// The target carries the mode too; the deny set it is compared with is
	// what its own Plan would refuse.
	var target *agents.Agent
	for _, h := range built.Agent.Handoffs {
		target = h.Target
	}
	if target == nil {
		t.Fatal("the entry agent has no handoff target")
	}
	planned, phase := built.plan().Apply(target)
	compare("target", planned, phase)
	// And a background build, which plan mode never wraps, asks the same.
	bg, err := buildFullAgent(context.Background(), runner.Deps, entry.ID, proj.ID, true, store.LocalUserID)
	if err != nil {
		t.Fatal(err)
	}
	defer bg.Release()
	if bg.PlanPhase != nil {
		t.Fatal("a background build was put in plan mode")
	}
	if !asks(t, toolNamed(t, bg.Agent.Tools, "write_file"), nil) || asks(t, toolNamed(t, bg.Agent.Tools, "read_file"), nil) {
		t.Error("a background run does not follow the agent's approval mode")
	}
}

// An MCP tool is a change unless the server's config names it, whatever the
// server says about it; the comparison holds through the listing wrapper.
func TestApprovalModeOnChangeFollowsTheMCPAllowList(t *testing.T) {
	hinted := noopTool("docs__hinted")
	hinted.ReadOnly = true // the server's readOnlyHint
	srv := fakeMCPServer{name: "docs", tools: []*agents.Tool{noopTool("docs__search"), hinted, noopTool("docs__write")}}
	build := func() *BuildResult {
		return &BuildResult{
			Agent:        &agents.Agent{Name: "a", MCPServers: []agents.MCPServer{srv}},
			PlanReadOnly: []string{"docs__search"},
			Approval:     store.ApprovalGroup{Mode: store.ApprovalModeOnChange},
		}
	}
	r := build()
	applyApprovalMode(r)
	planned, phase := r.plan().Apply(r.Agent)
	listed, err := planned.MCPServers[0].ListTools(context.Background(), nil, planned)
	if err != nil {
		t.Fatal(err)
	}
	denied := map[string]bool{}
	for _, tool := range listed {
		denied[tool.Name] = deniedWhilePlanning(t, tool)
	}
	if err := phase.Unlock(); err != nil {
		t.Fatal(err)
	}
	for _, tool := range listed {
		if got := asks(t, tool, nil); got != denied[tool.Name] {
			t.Errorf("%s: asked=%v denied=%v", tool.Name, got, denied[tool.Name])
		}
	}
	if !denied["docs__hinted"] || !denied["docs__write"] || denied["docs__search"] {
		t.Fatalf("denied = %v, want the hint ignored and the listed name admitted", denied)
	}
}

// always asks about every tool, reads included, and exec_command still goes
// through the per-command gate: a command the session trusts is not asked.
func TestApprovalModeAlwaysKeepsPerCommandExec(t *testing.T) {
	runner, entry, proj := approvalModeRunner(t, store.ApprovalModeAlways)
	built, err := buildFullAgent(context.Background(), runner.Deps, entry.ID, proj.ID, true, store.LocalUserID)
	if err != nil {
		t.Fatal(err)
	}
	defer built.Release()
	if slices.Contains(built.Agent.ApproveTools, "*") {
		t.Fatal(`always was written as ApproveTools ["*"]`)
	}
	for _, name := range []string{"read_file", "list_files", "apply_patch", "write_file"} {
		if tool := toolNamed(t, built.Agent.Tools, name); !asks(t, tool, nil) {
			t.Errorf("%s is not asked about under always", name)
		}
	}
	exec := toolNamed(t, built.Agent.Tools, execCommandToolName)
	if !asks(t, exec, &agents.RunContext{Context: "s1"}) {
		t.Fatal("an untrusted command is not asked about")
	}
	runner.Deps.SandboxManager.Trust().ForSession("s1").AllowAll()
	if asks(t, exec, &agents.RunContext{Context: "s1"}) {
		t.Fatal("a trusted command is still asked about: exec_command lost its per-command gate")
	}
}

// An empty mode (a row from before the field) and never behave alike: only
// the list asks, exec_command's listing is the per-command gate, and no
// predicate is installed anywhere else.
func TestEmptyModeKeepsLegacyBehavior(t *testing.T) {
	for _, mode := range []string{"", store.ApprovalModeNever} {
		runner, entry, proj := approvalModeRunner(t, mode, "write_file", execCommandToolName)
		built, err := buildFullAgent(context.Background(), runner.Deps, entry.ID, proj.ID, true, store.LocalUserID)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(built.Agent.ApproveTools, []string{"write_file"}) {
			t.Fatalf("mode %q: ApproveTools = %v, want the list minus exec_command", mode, built.Agent.ApproveTools)
		}
		for _, name := range []string{"apply_patch", "read_file", "write_file"} {
			if tool := toolNamed(t, built.Agent.Tools, name); tool.NeedsApprovalFunc != nil || tool.NeedsApproval {
				t.Errorf("mode %q: %s carries a predicate", mode, name)
			}
		}
		if exec := toolNamed(t, built.Agent.Tools, execCommandToolName); exec.NeedsApprovalFunc == nil {
			t.Errorf("mode %q: the listed exec_command has no per-command gate", mode)
		}
		built.Release()
	}
	// With nothing listed, never installs nothing at all.
	runner, entry, proj := approvalModeRunner(t, store.ApprovalModeNever)
	built, err := buildFullAgent(context.Background(), runner.Deps, entry.ID, proj.ID, true, store.LocalUserID)
	if err != nil {
		t.Fatal(err)
	}
	defer built.Release()
	if exec := toolNamed(t, built.Agent.Tools, execCommandToolName); exec.NeedsApprovalFunc != nil {
		t.Error("never with an empty list gates exec_command")
	}
}

func noopTool(name string) *agents.Tool {
	return agents.NewTool(name, "test tool", func(context.Context, *agents.ToolContext, struct{}) (string, error) { return "ok", nil })
}

type fakeMCPServer struct {
	name  string
	tools []*agents.Tool
}

func (f fakeMCPServer) Name() string { return f.name }
func (f fakeMCPServer) Close() error { return nil }
func (f fakeMCPServer) ListTools(context.Context, *agents.RunContext, *agents.Agent) ([]*agents.Tool, error) {
	return slices.Clone(f.tools), nil
}
