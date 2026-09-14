package bridge

import (
	"context"
	"encoding/json"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/cmd/agents-server/internal/guardrails"
	"github.com/zzir/agents-go/cmd/agents-server/internal/mcpservers"
	"github.com/zzir/agents-go/cmd/agents-server/internal/sandboxes"
	"github.com/zzir/agents-go/cmd/agents-server/internal/settings"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/cmd/agents-server/internal/testdb"
	"github.com/zzir/agents-go/sandbox"
)

// buildAgentRegistry resolves the names in a serialized RunState back to live
// agents, and that resolved agent is the one the SDK re-runs on approval. So
// the registry MUST carry the run's sandbox-backed tools; building it with an
// empty project id strips exec_command/read_file/… and the approved call fails
// with "tool not found on agent" (regression: an approval-gated sandbox tool
// could never be approved).
func TestBuildAgentRegistryIncludesSandboxTools(t *testing.T) {
	ctx := context.Background()
	db := testdb.New(t)

	agentConfigs := store.NewAgentConfigStore(db)
	targets := store.NewSandboxStore(db)

	ac := &store.AgentConfig{OwnerID: store.LocalUserID, Name: "coder", Model: "gpt-test", ProviderID: testProvider(t, db, "p", "sk-x", "")}
	if err := agentConfigs.Create(ctx, ac); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	// Building tools never contacts the daemon, so a plain docker pair works.
	tg := &store.Sandbox{ID: store.NewID(), Name: "L", Type: "docker", Config: []byte(`{"image":"i"}`)}
	if err := targets.Create(ctx, tg); err != nil {
		t.Fatalf("create target: %v", err)
	}

	runner := NewRunner(ctx, db, &AgentDeps{
		AgentConfigs:   agentConfigs,
		Providers:      store.NewProviderStore(db),
		Sandboxes:      targets,
		Settings:       settings.NewReader(store.NewSettingStore(db)),
		Memories:       store.NewMemoryStore(db),
		McpServers:     store.NewMcpServerStore(db),
		Guardrails:     guardrails.NewResolver(store.NewGuardrailStore(db)),
		McpManager:     mcpservers.NewManager(ctx, settings.NewReader(store.NewSettingStore(db))),
		SandboxManager: sandboxes.NewManager(),
		Projects:       store.NewProjectStore(db),
	})
	proj := &store.Project{OwnerID: store.LocalUserID, SandboxID: tg.ID, Name: "p"}
	if err := runner.Deps.Projects.Create(ctx, proj); err != nil {
		t.Fatalf("create project: %v", err)
	}

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

	// With the project id: the resolved agent must carry exec_command.
	withSb, _, err := runner.buildAgentRegistry(ctx, ac.ID, proj.ID, false, "")
	if err != nil {
		t.Fatalf("buildAgentRegistry(project, false): %v", err)
	}
	if !hasExec(withSb) {
		t.Error("registry built with a project id is missing exec_command")
	}

	// Without it, exec_command is absent — this is exactly the state that
	// stranded approvals, so the fix is that ResolveApproval passes the id.
	noSb, _, err := runner.buildAgentRegistry(ctx, ac.ID, "", false, "")
	if err != nil {
		t.Fatalf("buildAgentRegistry(none, false): %v", err)
	}
	if hasExec(noSb) {
		t.Error("registry built with no project id unexpectedly has exec_command")
	}
}

// A resumed run executes on the agent the approval path rebuilt: one sandbox
// acquire per decision, not one for the registry and a second for the segment.
func TestResumeBuildsTheAgentOnce(t *testing.T) {
	ctx := context.Background()
	db := testdb.New(t)
	var tools []string
	srv := recordToolsModel(t, &tools)
	defer srv.Close()

	agentConfigs := store.NewAgentConfigStore(db)
	sessions := store.NewSessionStore(db)
	users := store.NewUserStore(db)
	if _, err := users.EnsureLocalUser(ctx); err != nil {
		t.Fatal(err)
	}
	targets := store.NewSandboxStore(db)
	ac := &store.AgentConfig{OwnerID: store.LocalUserID, Name: "approver", Model: "gpt-test", ProviderID: testProvider(t, db, "p", "sk-x", srv.URL)}
	if err := agentConfigs.Create(ctx, ac); err != nil {
		t.Fatal(err)
	}
	tg := &store.Sandbox{ID: store.NewID(), Name: "L", Type: "docker", Config: []byte(`{"image":"i"}`)}
	if err := targets.Create(ctx, tg); err != nil {
		t.Fatal(err)
	}
	mgr := sandboxes.NewManager()
	mgr.SetBuildOverride(func(sandboxes.Spec) (sandbox.Sandbox, error) { return sandbox.NewLocal(), nil })
	// Every Acquire ends in exactly one release, and a release reads the idle
	// window before anything else — so the window provider counts holders.
	var releases atomic.Int32
	mgr.SetIdleTimeout(func() time.Duration { releases.Add(1); return 0 })

	runner := NewRunner(ctx, db, &AgentDeps{
		AgentConfigs:     agentConfigs,
		Providers:        store.NewProviderStore(db),
		Sessions:         sessions,
		Users:            users,
		Sandboxes:        targets,
		Projects:         store.NewProjectStore(db),
		Settings:         settings.NewReader(store.NewSettingStore(db)),
		Memories:         store.NewMemoryStore(db),
		PendingApprovals: store.NewPendingApprovalStore(db),
		SandboxManager:   mgr,
		Traces:           store.NewTraceStore(db),
	})
	proj := &store.Project{OwnerID: store.LocalUserID, SandboxID: tg.ID, Name: "p"}
	if err := runner.Deps.Projects.Create(ctx, proj); err != nil {
		t.Fatal(err)
	}
	sess := &store.Session{OwnerID: store.LocalUserID, ID: store.NewID(), Name: "s", ProjectID: proj.ID}
	if err := sessions.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}

	args := `{"cmd":"echo ok","timeout_seconds":0,"workdir":"","session_id":""}`
	var rawCall agents.OutputItem
	if err := json.Unmarshal([]byte(`{"type":"function_call","call_id":"call-once-1","name":"exec_command","arguments":`+strconv.Quote(args)+`}`), &rawCall); err != nil {
		t.Fatal(err)
	}
	state := &agents.RunState{
		CurrentAgent: &agents.Agent{Name: "approver"},
		Approvals:    agents.NewApprovalStore(),
		UserInput:    userInputItems(t, `{"role":"user","content":"run it"}`),
		Interruptions: []*agents.ToolApprovalItem{{
			Agent: &agents.Agent{Name: "approver"}, ToolName: "exec_command", CallID: "call-once-1", Raw: rawCall,
		}},
	}
	stateJSON, err := state.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	calls, _ := json.Marshal([]store.PendingToolCall{{ToolCallID: "call-once-1", ToolName: "exec_command", Arguments: args}})
	if err := runner.Deps.PendingApprovals.Save(ctx, &store.PendingApproval{
		RunID: "paused-run", SessionID: sess.ID, AgentConfigID: ac.ID, ProjectID: proj.ID, State: string(stateJSON), ToolCalls: calls,
	}); err != nil {
		t.Fatal(err)
	}

	done := make(chan *RunOutcome, 1)
	if _, _, err := runner.ResolveApproval(ctx, "call-once-1", true, ApprovalOnce, "", func(res *RunOutcome) { done <- res }); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	var res *RunOutcome
	select {
	case res = <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the resumed run did not finish")
	}
	if res.ErrCode != "" || res.FinalText != "done" {
		t.Fatalf("resumed run = %q %q / %q, want it completed with the model's answer", res.ErrCode, res.ErrMessage, res.FinalText)
	}
	if got := releases.Load(); got != 1 {
		t.Fatalf("the sandbox was acquired %d times over one resume, want 1", got)
	}
}
