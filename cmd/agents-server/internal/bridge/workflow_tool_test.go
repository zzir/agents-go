package bridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// requestToolNames reads the tool names a Responses request offered.
func requestToolNames(t *testing.T, r *http.Request) []string {
	t.Helper()
	var body struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Fatalf("decoding the model request: %v", err)
	}
	names := make([]string, 0, len(body.Tools))
	for _, tool := range body.Tools {
		names = append(names, tool.Name)
	}
	return names
}

// recordToolsModel records the tools of the first call and then answers.
func recordToolsModel(t *testing.T, tools *[]string) *httptest.Server {
	t.Helper()
	var calls atomic.Int32
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			*tools = requestToolNames(t, r)
		}
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
}

// startWorkflowModel calls start_workflow on its first turn and answers on
// every turn after — including the workflow's own steps.
func startWorkflowModel(t *testing.T, name string, tools *[]string) *httptest.Server {
	t.Helper()
	var calls atomic.Int32
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n := calls.Add(1); n == 1 {
			*tools = requestToolNames(t, r)
		}
		send := sseWriter(w)
		sseCreated(send)
		var output []any
		if calls.Load() == 1 {
			output = []any{map[string]any{
				"type": "function_call", "id": "fc_1", "call_id": "call_1",
				"name": StartWorkflowToolName, "arguments": `{"name":"` + name + `","input":"do the thing"}`, "status": "completed",
			}}
		} else {
			output = []any{map[string]any{
				"type": "message", "id": "msg_1", "status": "completed", "role": "assistant",
				"content": []any{map[string]any{"type": "output_text", "text": "ok", "annotations": []any{}}},
			}}
		}
		send("response.completed", map[string]any{
			"type": "response.completed", "sequence_number": 1,
			"response": map[string]any{
				"id": "resp_1", "object": "response", "created_at": 0, "status": "completed", "model": "gpt-test",
				"output": output,
				"usage":  map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2},
			},
		})
	}))
}

// The agent starts a workflow: the tool is offered, the call starts the
// sequence at once — it runs on a session of its own, so the turn asking for it
// is not in the way — and the execution belongs to the conversation that asked.
func TestModelStartsAWorkflow(t *testing.T) {
	ctx := context.Background()
	var offered []string
	srv := startWorkflowModel(t, "codegen", &offered)
	defer srv.Close()

	runner, sessions, _, agentConfigs := newTaskTestRunner(t)
	runner.Deps.Workflows = store.NewWorkflowStore(runner.db)
	runner.Deps.WorkflowRuns = store.NewWorkflowRunStore(runner.db)
	ac := &store.AgentConfig{
		Name: "chat", Model: "gpt-test",
		ProviderID: testProvider(t, runner.db, "endpoint", "k", srv.URL),
	}
	if err := agentConfigs.Create(ctx, ac); err != nil {
		t.Fatal(err)
	}
	wf := &store.Workflow{
		Name: "codegen", Description: "Implement a feature end to end",
		Steps: store.WorkflowSteps{{AgentConfigID: ac.ID, Prompt: "Write the code."}},
	}
	if err := store.NormalizeWorkflow(wf); err != nil {
		t.Fatal(err)
	}
	if err := runner.Deps.Workflows.Create(ctx, wf); err != nil {
		t.Fatal(err)
	}
	sess := &store.Session{ID: store.NewID(), Name: "chat"}
	if err := sessions.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	if _, err := runner.StartRun(sess.ID, ac.ID, "", "", "add a feature", nil, func(*RunOutcome) { close(done) }); err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the chat run never finished")
	}
	if !slices.Contains(offered, StartWorkflowToolName) {
		t.Fatalf("tools = %v, want %s offered", offered, StartWorkflowToolName)
	}

	// The sequence starts from the teardown, so it appears just after the run.
	deadline := time.Now().Add(15 * time.Second)
	var wr *store.WorkflowRun
	for {
		list, err := runner.Deps.WorkflowRuns.ListBySession(ctx, sess.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(list) > 0 && list[0].Status != store.WorkflowRunning {
			wr = &list[0]
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no finished execution on the session (got %d)", len(list))
		}
		time.Sleep(20 * time.Millisecond)
	}
	if wr.Name != "codegen" || wr.Status != store.WorkflowCompleted {
		t.Fatalf("execution = %q %q, want codegen completed (%s)", wr.Name, wr.Status, wr.Error)
	}
}

// With no workflows the tool is absent entirely — an empty chooser is a tool
// that can only be called wrongly.
func TestStartWorkflowToolAbsentWithoutWorkflows(t *testing.T) {
	ctx := context.Background()
	runner, _, _, agentConfigs := newTaskTestRunner(t)
	runner.Deps.Workflows = store.NewWorkflowStore(runner.db)
	ac := &store.AgentConfig{Name: "chat", Model: "gpt-test"}
	if err := agentConfigs.Create(ctx, ac); err != nil {
		t.Fatal(err)
	}
	if tool := runner.startWorkflowTool(ctx); tool != nil {
		t.Fatal("no workflows at all: the tool must be absent")
	}
	wf := &store.Workflow{Name: "deploy", Description: "ship it", Steps: store.WorkflowSteps{{AgentConfigID: ac.ID, Prompt: "ship"}}}
	if err := store.NormalizeWorkflow(wf); err != nil {
		t.Fatal(err)
	}
	if err := runner.Deps.Workflows.Create(ctx, wf); err != nil {
		t.Fatal(err)
	}
	if tool := runner.startWorkflowTool(ctx); tool == nil {
		t.Fatal("a workflow exists: the tool must be offered")
	}
}

// A workflow STEP is a background run: no plan mode, no todo, no task tools,
// no start_workflow. Plan mode is the one that deadlocks — submit_plan pauses
// for an approval, and a step's approval lands in a session nobody can open, so
// the sequence would wait forever on a decision nobody can see.
func TestWorkflowStepIsBuiltAsABackgroundRun(t *testing.T) {
	ctx := context.Background()
	var offered []string
	srv := recordToolsModel(t, &offered)
	defer srv.Close()

	runner, sessions, _, agentConfigs := newTaskTestRunner(t)
	runner.Deps.Workflows = store.NewWorkflowStore(runner.db)
	runner.Deps.WorkflowRuns = store.NewWorkflowRunStore(runner.db)
	ac := &store.AgentConfig{
		Name: "planner", Model: "gpt-test",
		ProviderID: testProvider(t, runner.db, "endpoint", "k", srv.URL),
	}
	if err := agentConfigs.Create(ctx, ac); err != nil {
		t.Fatal(err)
	}
	wf := &store.Workflow{
		Name: "codegen", Description: "write it",
		Steps: store.WorkflowSteps{{AgentConfigID: ac.ID, Prompt: "Write the code."}},
	}
	if err := store.NormalizeWorkflow(wf); err != nil {
		t.Fatal(err)
	}
	if err := runner.Deps.Workflows.Create(ctx, wf); err != nil {
		t.Fatal(err)
	}
	sess := &store.Session{ID: store.NewID(), Name: "chat"}
	if err := sessions.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}

	wr, err := runner.StartWorkflow(ctx, wf.ID, sess.ID, "the brief")
	if err != nil {
		t.Fatalf("StartWorkflow: %v", err)
	}
	if done := awaitWorkflow(t, runner, wr.ID, 15*time.Second); done.Status != store.WorkflowCompleted {
		t.Fatalf("status = %q (%s), want completed — a step must not stop for a plan review", done.Status, done.Error)
	}
	for _, name := range []string{"submit_plan", "todo_write", "spawn_task", StartWorkflowToolName} {
		if slices.Contains(offered, name) {
			t.Errorf("a workflow step was offered %q; its toolset = %v", name, offered)
		}
	}
}

// The status report is BOUNDED: a conversation that runs many workflows must
// not grow this tool's output without limit. Live ones are never dropped —
// redoing work in flight is the failure this tool exists to prevent.
func TestWorkflowStatusReportKeepsLiveDropsOldFinished(t *testing.T) {
	runs := []store.WorkflowRun{
		{Name: "live-a", Status: store.WorkflowRunning},
		{Name: "done-1", Status: store.WorkflowCompleted},
		{Name: "done-2", Status: store.WorkflowCompleted},
		{Name: "done-3", Status: store.WorkflowFailed, Error: "boom"},
		{Name: "done-4", Status: store.WorkflowCompleted},
		{Name: "done-5", Status: store.WorkflowCancelled},
		{Name: "live-b", Status: store.WorkflowRunning},
	}
	got := workflowStatusReport(runs)
	for _, name := range []string{"live-a", "live-b", "done-1", "done-2", "done-3"} {
		if !strings.Contains(got, name) {
			t.Errorf("%q missing from the report:\n%s", name, got)
		}
	}
	for _, name := range []string{"done-4", "done-5"} {
		if strings.Contains(got, name) {
			t.Errorf("%q should have been left out:\n%s", name, got)
		}
	}
	// What was dropped is said, not silently swallowed.
	if !strings.Contains(got, "2 older finished") {
		t.Errorf("the report must own up to what it omitted:\n%s", got)
	}
}
