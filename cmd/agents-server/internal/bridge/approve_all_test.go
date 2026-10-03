package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/middleware"
	"github.com/zzir/agents-go/cmd/agents-server/internal/sandboxes"
	"github.com/zzir/agents-go/cmd/agents-server/internal/settings"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/cmd/agents-server/internal/testdb"
	"github.com/zzir/agents-go/sandbox"
)

// pausedOn writes a pause of the session's run on the given exec_command
// calls (plus any extra interruptions), as the runner would have.
func pausedOn(t *testing.T, runner *Runner, sess *store.Session, ac *store.AgentConfig, proj *store.Project, callIDs []string, extra ...*agents.ToolApprovalItem) string {
	t.Helper()
	args := `{"cmd":"echo ok","timeout_seconds":0,"workdir":"","session_id":""}`
	var items []*agents.ToolApprovalItem
	var calls []store.PendingToolCall
	for _, id := range callIDs {
		var raw agents.OutputItem
		if err := json.Unmarshal([]byte(`{"type":"function_call","call_id":"`+id+`","name":"exec_command","arguments":`+strconv.Quote(args)+`}`), &raw); err != nil {
			t.Fatal(err)
		}
		items = append(items, &agents.ToolApprovalItem{Agent: &agents.Agent{Name: ac.Name}, ToolName: "exec_command", CallID: id, Raw: raw})
		calls = append(calls, store.PendingToolCall{ToolCallID: id, ToolName: "exec_command", Arguments: args})
	}
	for _, it := range extra {
		items = append(items, it)
		calls = append(calls, store.PendingToolCall{ToolCallID: it.CallID, ToolName: it.ToolName, Arguments: "{}"})
	}
	state := &agents.RunState{
		CurrentAgent:  &agents.Agent{Name: ac.Name},
		Approvals:     agents.NewApprovalStore(),
		UserInput:     userInputItems(t, `{"role":"user","content":"run it"}`),
		Interruptions: items,
	}
	stateJSON, err := state.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	callsJSON, _ := json.Marshal(calls)
	runID := store.NewID()
	if err := runner.Deps.PendingApprovals.Save(context.Background(), &store.PendingApproval{
		RunID: runID, SessionID: sess.ID, AgentConfigID: ac.ID, ProjectID: proj.ID, State: string(stateJSON), ToolCalls: callsJSON,
	}); err != nil {
		t.Fatal(err)
	}
	return runID
}

// One Approve all on a pause of several calls resumes the run once, with
// every call approved; a call a person confirms one by one (a plan) is left
// out, and a pause made only of those is refused before anything is claimed.
func TestApproveAllResumesOnce(t *testing.T) {
	ctx := context.Background()
	db := testdb.New(t)
	var modelCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		modelCalls.Add(1)
		send := sseWriter(w)
		sseCreated(send)
		send("response.completed", map[string]any{
			"type": "response.completed", "sequence_number": 1,
			"response": map[string]any{
				"id": "resp_1", "object": "response", "created_at": 0, "status": "completed", "model": "gpt-test",
				"output": []any{map[string]any{
					"type": "message", "id": "msg_1", "status": "completed", "role": "assistant",
					"content": []any{map[string]any{"type": "output_text", "text": "done", "annotations": []any{}}},
				}},
				"usage": map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2},
			},
		})
	}))
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
	runner := NewRunner(ctx, db, &AgentDeps{
		AgentConfigs: agentConfigs, Providers: store.NewProviderStore(db), Sessions: sessions, Users: users,
		Sandboxes: targets, Projects: store.NewProjectStore(db), Settings: settings.NewReader(store.NewSettingStore(db)),
		Memories: store.NewMemoryStore(db), PendingApprovals: store.NewPendingApprovalStore(db), SandboxManager: mgr,
		Traces: store.NewTraceStore(db),
	})
	proj := &store.Project{OwnerID: store.LocalUserID, SandboxID: tg.ID, Name: "p"}
	if err := runner.Deps.Projects.Create(ctx, proj); err != nil {
		t.Fatal(err)
	}
	sess := &store.Session{OwnerID: store.LocalUserID, ID: store.NewID(), Name: "s", ProjectID: proj.ID}
	if err := sessions.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}

	// No pause: nothing to approve.
	if _, _, err := runner.ApproveAll(ctx, sess.ID, nil); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("approve-all with no pause: err = %v, want ErrNotFound", err)
	}

	// A pause made only of a plan: refused, the row kept.
	var planRaw agents.OutputItem
	if err := json.Unmarshal([]byte(`{"type":"function_call","call_id":"call-plan","name":"`+middleware.PlanToolName+`","arguments":"{\"plan\":\"x\"}"}`), &planRaw); err != nil {
		t.Fatal(err)
	}
	planRun := pausedOn(t, runner, sess, ac, proj, nil, &agents.ToolApprovalItem{Agent: &agents.Agent{Name: ac.Name}, ToolName: middleware.PlanToolName, CallID: "call-plan", Raw: planRaw})
	if _, _, err := runner.ApproveAll(ctx, sess.ID, nil); !errors.Is(err, ErrNothingToApproveAll) {
		t.Fatalf("approve-all on a plan-only pause: err = %v, want ErrNothingToApproveAll", err)
	}
	if rows, _ := runner.Deps.PendingApprovals.ListBySession(ctx, sess.ID); len(rows) != 1 {
		t.Fatalf("the refused pause was claimed: %d rows left", len(rows))
	}
	if err := runner.Deps.PendingApprovals.Delete(ctx, planRun); err != nil {
		t.Fatal(err)
	}

	// Two commands: both approved, one resume, one model call after it.
	pausedOn(t, runner, sess, ac, proj, []string{"call-a", "call-b"})
	done := make(chan *RunOutcome, 1)
	runID, n, err := runner.ApproveAll(ctx, sess.ID, func(res *RunOutcome) { done <- res })
	if err != nil {
		t.Fatalf("approve-all: %v", err)
	}
	if n != 2 || runID == "" {
		t.Fatalf("approve-all approved %d calls on run %q, want 2 on the resumed run", n, runID)
	}
	var res *RunOutcome
	select {
	case res = <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the resumed run did not finish")
	}
	if res.ErrCode != "" || res.FinalText != "done" {
		t.Fatalf("resumed run = %q %q / %q, want completed with the answer", res.ErrCode, res.ErrMessage, res.FinalText)
	}
	if got := modelCalls.Load(); got != 1 {
		t.Fatalf("the model was called %d times after one approve-all, want 1", got)
	}
	if rows, _ := runner.Deps.PendingApprovals.ListBySession(ctx, sess.ID); len(rows) != 0 {
		t.Fatalf("%d pauses left after the resume", len(rows))
	}
	// A second approve-all finds nothing: the one claim was the resume's.
	if _, _, err := runner.ApproveAll(ctx, sess.ID, nil); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("approve-all after the resume: err = %v, want ErrNotFound", err)
	}
}
