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

// startWorkflowModel calls spawn_task with a workflow on its first turn and
// answers on every turn after — including the workflow's own steps.
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
				"name": SpawnToolName, "arguments": `{"agent_name":"","workflow":"` + name + `","input":"do the thing","label":""}`, "status": "completed",
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
	ac := &store.AgentConfig{OwnerID: store.LocalUserID,
		Name: "chat", Model: "gpt-test",
		ProviderID: testProvider(t, runner.db, "endpoint", "k", srv.URL),
	}
	if err := agentConfigs.Create(ctx, ac); err != nil {
		t.Fatal(err)
	}
	wf := &store.Workflow{OwnerID: store.LocalUserID,
		Name: "codegen", Description: "Implement a feature end to end",
		Steps: store.WorkflowSteps{{AgentConfigID: ac.ID, Prompt: "Write the code."}},
	}
	if err := store.NormalizeWorkflow(wf); err != nil {
		t.Fatal(err)
	}
	if err := runner.Deps.Workflows.Create(ctx, wf); err != nil {
		t.Fatal(err)
	}
	sess := &store.Session{OwnerID: store.LocalUserID, ID: store.NewID(), Name: "chat"}
	if err := sessions.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	chatRunID, err := runner.StartRun(sess.ID, ac.ID, "", "add a feature", nil, func(*RunOutcome) { close(done) })
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the chat run never finished")
	}
	if !slices.Contains(offered, SpawnToolName) {
		t.Fatalf("tools = %v, want %s offered", offered, SpawnToolName)
	}
	// One vocabulary: no separate workflow tools beside the four task verbs.
	for _, gone := range []string{"start_workflow", "workflow_status"} {
		if slices.Contains(offered, gone) {
			t.Fatalf("tools = %v — %s must not exist", offered, gone)
		}
	}

	// The execution is a task of the asking session, started by the tool call
	// itself, so it is there before the run ends; here it has also finished.
	deadline := time.Now().Add(15 * time.Second)
	var wr *store.Task
	for {
		list, err := runner.Deps.Tasks.ListByParent(ctx, sess.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(list) > 0 && isTerminalTaskStatus(list[0].Status) {
			wr = &list[0]
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no finished execution on the session (got %d)", len(list))
		}
		time.Sleep(20 * time.Millisecond)
	}
	if wr.Kind != store.TaskKindWorkflow || wr.Label != "codegen" || wr.Status != "completed" {
		t.Fatalf("execution = %q %q %q, want a codegen workflow completed (%s)", wr.Kind, wr.Label, wr.Status, wr.Summary)
	}
	// The card that made the call is the one the execution reports through,
	// and the run that made it is the execution's lineage — what nests the
	// result's wake-up run under this run in the trace panel.
	if wr.ToolCallID != "call_1" {
		t.Fatalf("tool call = %q, want the spawn_task call", wr.ToolCallID)
	}
	if wr.ParentRunID != chatRunID {
		t.Fatalf("parent run = %q, want the run that called spawn_task (%s)", wr.ParentRunID, chatRunID)
	}
	// The wake-up run that delivers the result carries that lineage on its
	// spans.
	deadline = time.Now().Add(15 * time.Second)
	for {
		rows, err := runner.Deps.Traces.ListBySession(ctx, sess.ID, "", 0)
		if err != nil {
			t.Fatal(err)
		}
		var wake *store.TraceEvent
		for i := range rows {
			if rows[i].RunID != chatRunID && rows[i].RunID != wr.RunID {
				wake = &rows[i]
				break
			}
		}
		if wake != nil {
			if wake.ParentRunID != chatRunID {
				t.Fatalf("wake run %s spans carry parent %q, want %s", wake.RunID, wake.ParentRunID, chatRunID)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no wake-up run traced on the session")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// With no workflows the tool is absent entirely — an empty chooser is a tool
// that can only be called wrongly.
// spawn_task is always offered; the workflows on offer are in its DESCRIPTION,
// only when there are any — a server without workflows offers the plain tool.
func TestSpawnToolListsWorkflowsOnlyWhenThereAreAny(t *testing.T) {
	ctx := context.Background()
	runner, _, _, agentConfigs := newTaskTestRunner(t)
	runner.Deps.Workflows = store.NewWorkflowStore(runner.db)
	ac := &store.AgentConfig{OwnerID: store.LocalUserID, Name: "chat", Model: "gpt-test"}
	if err := agentConfigs.Create(ctx, ac); err != nil {
		t.Fatal(err)
	}
	tool := runner.spawnTool(ctx, "")
	if tool == nil || tool.Name != SpawnToolName || strings.Contains(tool.Description, "Available:") {
		t.Fatalf("no workflows: tool = %v, want spawn_task with no workflow list", tool)
	}
	wf := &store.Workflow{OwnerID: store.LocalUserID, Name: "deploy", Description: "ship it", Steps: store.WorkflowSteps{{AgentConfigID: ac.ID, Prompt: "ship"}}}
	if err := store.NormalizeWorkflow(wf); err != nil {
		t.Fatal(err)
	}
	if err := runner.Deps.Workflows.Create(ctx, wf); err != nil {
		t.Fatal(err)
	}
	tool = runner.spawnTool(ctx, "")
	if !strings.Contains(tool.Description, "- deploy: ship it") {
		t.Fatalf("a workflow exists: the description must list it, got %q", tool.Description)
	}
}

// task_status says where a workflow stands — the step it is on, the bound
// that stopped it — through the manager's DescribeState, so the model needs
// no second status tool for sequences.
func TestDescribeTaskStateSaysTheStep(t *testing.T) {
	st := &store.WorkflowState{
		Steps:  store.WorkflowSteps{{ID: "a", Name: "plan"}, {ID: "b", Name: "exec"}, {ID: "c"}},
		StepID: "b",
	}
	if got := describeTaskState(store.TaskKindWorkflow, st.Encode()); got != "step 2/3 (exec)" {
		t.Fatalf("progress = %q", got)
	}
	st.StepID, st.Stopped = "c", store.StoppedByBudget
	if got := describeTaskState(store.TaskKindWorkflow, st.Encode()); got != "step 3/3, stopped by its budget" {
		t.Fatalf("progress = %q", got)
	}
	// A sequence that came back to a step has more runs than steps: said, and
	// the lap bound that stops such a loop is named.
	st.StepID, st.Stopped = "b", ""
	for _, id := range []string{"a", "b", "c", "b"} {
		st.StepRuns = st.StepRuns.With(id, store.NewID())
	}
	if got := describeTaskState(store.TaskKindWorkflow, st.Encode()); got != "step 2/3 (exec), run 4" {
		t.Fatalf("progress = %q", got)
	}
	// A person's retry of a step is one more run, not the sequence looping:
	// a linear run of three steps retried once is not "run 4".
	linear := &store.WorkflowState{Steps: st.Steps, StepID: "c"}
	for _, id := range []string{"a", "b", "c"} {
		linear.StepRuns = linear.StepRuns.With(id, store.NewID())
	}
	linear.StepRuns = linear.StepRuns.WithRetry("c", store.NewID())
	if got := describeTaskState(store.TaskKindWorkflow, linear.Encode()); got != "step 3/3" {
		t.Fatalf("progress after a retry = %q, want no run count", got)
	}
	st.Stopped = store.StoppedByLaps
	if got := describeTaskState(store.TaskKindWorkflow, st.Encode()); got != "step 2/3 (exec), run 4, stopped by its loop bound" {
		t.Fatalf("progress = %q", got)
	}
	if got := describeTaskState("", nil); got != "" {
		t.Fatalf("a plain task has no progress line, got %q", got)
	}
}

// A workflow STEP is a background run: no plan mode, no todo, no task tools
// (spawn_task included). Plan mode is the one that deadlocks — submit_plan pauses
// for an approval, and a step's approval lands in a session nobody can open, so
// the sequence would wait forever on a decision nobody can see.
func TestWorkflowStepIsBuiltAsABackgroundRun(t *testing.T) {
	ctx := context.Background()
	var offered []string
	srv := recordToolsModel(t, &offered)
	defer srv.Close()

	runner, sessions, _, agentConfigs := newTaskTestRunner(t)
	runner.Deps.Workflows = store.NewWorkflowStore(runner.db)
	ac := &store.AgentConfig{OwnerID: store.LocalUserID,
		Name: "planner", Model: "gpt-test",
		ProviderID: testProvider(t, runner.db, "endpoint", "k", srv.URL),
	}
	if err := agentConfigs.Create(ctx, ac); err != nil {
		t.Fatal(err)
	}
	wf := &store.Workflow{OwnerID: store.LocalUserID,
		Name: "codegen", Description: "write it",
		Steps: store.WorkflowSteps{{AgentConfigID: ac.ID, Prompt: "Write the code."}},
	}
	if err := store.NormalizeWorkflow(wf); err != nil {
		t.Fatal(err)
	}
	if err := runner.Deps.Workflows.Create(ctx, wf); err != nil {
		t.Fatal(err)
	}
	sess := &store.Session{OwnerID: store.LocalUserID, ID: store.NewID(), Name: "chat"}
	if err := sessions.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}

	info, err := runner.StartWorkflow(ctx, wf.ID, sess.ID, "the brief", "")
	if err != nil {
		t.Fatalf("StartWorkflow: %v", err)
	}
	if done, _ := awaitWorkflow(t, runner, info.TaskID, 15*time.Second); done.Status != "completed" {
		t.Fatalf("status = %q (%s), want completed — a step must not stop for a plan review", done.Status, done.Summary)
	}
	for _, name := range []string{"submit_plan", "todo_write", "spawn_task", "task_status"} {
		if slices.Contains(offered, name) {
			t.Errorf("a workflow step was offered %q; its toolset = %v", name, offered)
		}
	}
}
