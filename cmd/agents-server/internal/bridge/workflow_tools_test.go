package bridge

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// recordingModel answers every call with what script returns for it (the
// call's 1-based index and its raw request body), recording per call the tool
// names offered — a chat run and the background runs it starts share one
// server, so a later index is a later run's request.
type recordingModel struct {
	mu     sync.Mutex
	calls  int
	tools  [][]string
	bodies [][]byte
	script func(call int, body []byte) []any
}

func newRecordingModel(t *testing.T, script func(call int, body []byte) []any) (*recordingModel, *httptest.Server) {
	t.Helper()
	m := &recordingModel{script: script}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		}
		_ = json.Unmarshal(body, &req)
		names := make([]string, 0, len(req.Tools))
		for _, tool := range req.Tools {
			names = append(names, tool.Name)
		}
		m.mu.Lock()
		m.calls++
		call := m.calls
		m.tools = append(m.tools, names)
		m.bodies = append(m.bodies, body)
		m.mu.Unlock()

		send := sseWriter(w)
		sseCreated(send)
		send("response.completed", map[string]any{
			"type": "response.completed", "sequence_number": 1,
			"response": map[string]any{
				"id": "resp_1", "object": "response", "created_at": 0, "status": "completed", "model": "gpt-test",
				"output": m.script(call, body),
				"usage":  map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2},
			},
		})
	}))
	t.Cleanup(srv.Close)
	return m, srv
}

func (m *recordingModel) toolsOfCall(i int) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if i < 1 || i > len(m.tools) {
		return nil
	}
	return m.tools[i-1]
}

func (m *recordingModel) bodyOfCall(i int) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if i < 1 || i > len(m.bodies) {
		return ""
	}
	return string(m.bodies[i-1])
}

// toolsOfBody waits for the first request whose body contains needle and
// returns the tools it offered — how a background run's request is told apart
// from the chat run's turns.
func (m *recordingModel) toolsOfBody(t *testing.T, needle string, wait time.Duration) []string {
	t.Helper()
	deadline := time.Now().Add(wait)
	for {
		m.mu.Lock()
		for i, body := range m.bodies {
			if strings.Contains(string(body), needle) {
				tools := m.tools[i]
				m.mu.Unlock()
				return tools
			}
		}
		m.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatalf("no model request carried %q", needle)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func sayOutput(text string) []any {
	return []any{map[string]any{
		"type": "message", "id": "msg_1", "status": "completed", "role": "assistant",
		"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}},
	}}
}

func callOutput(callID, name, args string) []any {
	return []any{map[string]any{
		"type": "function_call", "id": "fc_" + callID, "call_id": callID,
		"name": name, "arguments": args, "status": "completed",
	}}
}

// workflowToolsFixture is a runner with the workflow store, three agents
// (planner / coder / reviewer, coder opted into authoring) on the given model
// endpoint, and a chat session.
func workflowToolsFixture(t *testing.T, modelURL string) (runner *Runner, sess *store.Session, coder *store.AgentConfig) {
	t.Helper()
	ctx := context.Background()
	var sessions *store.SessionStore
	var agentConfigs *store.AgentConfigStore
	runner, sessions, _, agentConfigs = newTaskTestRunner(t)
	runner.Deps.Workflows = store.NewWorkflowStore(runner.db)
	pid := testProvider(t, runner.db, "endpoint", "k", modelURL)
	for _, name := range []string{"planner", "coder", "reviewer"} {
		ac := &store.AgentConfig{Name: name, Model: "gpt-test", ProviderID: pid}
		if name == "coder" {
			ac.Behavior.WorkflowAuthoring = true
			coder = ac
		}
		if err := agentConfigs.Create(ctx, ac); err != nil {
			t.Fatal(err)
		}
	}
	sess = &store.Session{OwnerID: store.LocalUserID, ID: store.NewID(), Name: "chat"}
	if err := sessions.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	return runner, sess, coder
}

func toolNamed(t *testing.T, tools []*agents.Tool, name string) *agents.Tool {
	t.Helper()
	for _, tool := range tools {
		if tool.Name == name {
			return tool
		}
	}
	t.Fatalf("no tool %q among %d", name, len(tools))
	return nil
}

func invokeText(t *testing.T, tool *agents.Tool, args string) string {
	t.Helper()
	out, err := tool.OnInvoke(context.Background(), &agents.ToolContext{}, args)
	if err != nil {
		t.Fatalf("%s(%s): %v", tool.Name, args, err)
	}
	s, _ := out.ModelOutput().(string)
	return s
}

func runChat(t *testing.T, runner *Runner, sess *store.Session, ac *store.AgentConfig, input string) *RunOutcome {
	t.Helper()
	done := make(chan *RunOutcome, 1)
	if _, err := runner.StartRun(sess.ID, ac.ID, "", "", input, nil, func(o *RunOutcome) { done <- o }); err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	select {
	case o := <-done:
		return o
	case <-time.After(15 * time.Second):
		t.Fatal("the chat run never finished")
		return nil
	}
}

const buildSpec = `{"name":"build","description":"Implement a feature end to end, with tests",
 "steps":[
  {"name":"plan","agent":"planner","prompt":"Write a plan.","gate":false,"gate_pass":"","gate_fail":"","pause_before":false,"compact_before":false,"on_success":"","on_failure":""},
  {"name":"exec","agent":"coder","prompt":"Carry out the plan.","gate":false,"gate_pass":"","gate_fail":"","pause_before":false,"compact_before":false,"on_success":"","on_failure":""},
  {"name":"verify","agent":"reviewer","prompt":"Run the tests.","gate":true,"gate_pass":"","gate_fail":"","pause_before":false,"compact_before":true,"on_success":"end","on_failure":"exec"}],
 "budget":{"max_steps":0,"max_tokens":0,"max_minutes":0,"max_laps":2}}`

// The pair is offered to a chat run of an agent that opted in, to no other
// agent, and never to a background run — a step's agent may have opted in,
// but a step is a task's run and nobody would answer its approval.
func TestWorkflowAuthoringToolsAreOptInAndChatOnly(t *testing.T) {
	ctx := context.Background()
	model, srv := newRecordingModel(t, func(call int, _ []byte) []any {
		if call == 1 {
			return callOutput("call_1", SpawnToolName, `{"agent_name":"","workflow":"build","input":"go","label":""}`)
		}
		return sayOutput("ok")
	})
	runner, sess, coder := workflowToolsFixture(t, srv.URL)
	// A one-step workflow whose step runs the opted-in agent.
	wf := &store.Workflow{Name: "build", Description: "Build it", Steps: store.WorkflowSteps{{AgentConfigID: coder.ID, Prompt: "Do it."}}}
	if err := store.NormalizeWorkflow(wf); err != nil {
		t.Fatal(err)
	}
	if err := runner.Deps.Workflows.Create(ctx, wf); err != nil {
		t.Fatal(err)
	}

	runChat(t, runner, sess, coder, "start build")
	chatTools := model.toolsOfCall(1)
	for _, want := range []string{WorkflowGetToolName, WorkflowSaveToolName, SpawnToolName} {
		if !slices.Contains(chatTools, want) {
			t.Fatalf("chat run tools = %v, want %s", chatTools, want)
		}
	}
	// The execution's step run is the request that carries the step's prompt
	// (the chat run's own second turn, with the spawn result, comes first).
	stepTools := model.toolsOfBody(t, "Do it.", 15*time.Second)
	for _, gone := range []string{WorkflowGetToolName, WorkflowSaveToolName, SpawnToolName} {
		if slices.Contains(stepTools, gone) {
			t.Fatalf("background step tools = %v — %s must not be offered", stepTools, gone)
		}
	}
	if len(stepTools) != 0 {
		t.Fatalf("background step tools = %v, want none (no sandbox, no chat surface)", stepTools)
	}

	// An agent that did not opt in has neither, spawn_task as ever.
	planner, err := runner.agentConfigByName(ctx, "", "planner")
	if err != nil {
		t.Fatal(err)
	}
	other := &store.Session{OwnerID: store.LocalUserID, ID: store.NewID(), Name: "chat 2"}
	if err := runner.Deps.Sessions.Create(ctx, other); err != nil {
		t.Fatal(err)
	}
	runChat(t, runner, other, planner, "hello planner")
	// By content: the first session's wake-up run (the opted-in agent's) may
	// still be arriving.
	plannerTools := model.toolsOfBody(t, "hello planner", time.Second)
	if slices.Contains(plannerTools, WorkflowSaveToolName) || slices.Contains(plannerTools, WorkflowGetToolName) || !slices.Contains(plannerTools, SpawnToolName) {
		t.Fatalf("planner tools = %v, want spawn_task without the authoring pair", plannerTools)
	}
}

// save_workflow creates by name and updates by name; agents and edges are
// resolved from names to ids, and a step that keeps its name keeps its id.
func TestSaveWorkflowCreatesAndUpdatesByName(t *testing.T) {
	ctx := context.Background()
	_, srv := newRecordingModel(t, func(int, []byte) []any { return sayOutput("ok") })
	runner, _, _ := workflowToolsFixture(t, srv.URL)
	// The one write to shared configuration that happens through a tool is
	// audited, attributed to the session's owner.
	var audited []protocol.AuditRecord
	runner.Deps.Audit = func(_ context.Context, r protocol.AuditRecord) { audited = append(audited, r) }
	tools := runner.workflowTools(ctx, store.LocalUserID)
	save := toolNamed(t, tools, WorkflowSaveToolName)
	if !save.NeedsApproval || save.ReadOnly {
		t.Fatalf("save_workflow: NeedsApproval=%v ReadOnly=%v, want an approval-gated writer", save.NeedsApproval, save.ReadOnly)
	}
	if !toolNamed(t, tools, WorkflowGetToolName).ReadOnly {
		t.Fatal("get_workflow must be read-only")
	}
	if !strings.Contains(save.Description, "coder, planner, reviewer") {
		t.Fatalf("description = %q, want the agents on offer", save.Description)
	}

	out := invokeText(t, save, buildSpec)
	if !strings.HasPrefix(out, `Created workflow "build" (3 steps)`) || !strings.Contains(out, "spawn_task(workflow=") {
		t.Fatalf("create said %q", out)
	}
	list, err := runner.Deps.Workflows.List(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("workflows = %d (%v), want 1", len(list), err)
	}
	stored := list[0]
	if len(audited) != 1 || audited[0].Action != "workflow.save" || audited[0].Resource != stored.ID || audited[0].Actor.ID != store.LocalUserID || !strings.Contains(audited[0].Detail, "created") {
		t.Fatalf("audit after a save = %+v, want workflow.save on the new id by the owner", audited)
	}
	if stored.Description != "Implement a feature end to end, with tests" || stored.Budget.MaxLaps != 2 || len(stored.Steps) != 3 {
		t.Fatalf("stored = %+v", stored)
	}
	coder, _ := runner.agentConfigByName(ctx, "", "coder")
	reviewer, _ := runner.agentConfigByName(ctx, "", "reviewer")
	plan, exec, verify := stored.Steps[0], stored.Steps[1], stored.Steps[2]
	if plan.ID == "" || exec.ID == "" || verify.ID == "" || plan.ID == exec.ID {
		t.Fatalf("step ids not assigned: %+v", stored.Steps)
	}
	if exec.AgentConfigID != coder.ID || verify.AgentConfigID != reviewer.ID {
		t.Fatalf("agents not resolved by name: %+v", stored.Steps)
	}
	if verify.OnSuccess != store.WorkflowStepEnd || verify.OnFailure != exec.ID || verify.Gate == nil || !verify.CompactBefore {
		t.Fatalf("verify step = %+v, want gate, on_success end, on_failure → exec's id", verify)
	}

	// Update: verify's prompt changes and a fourth step lands; the three kept
	// steps keep their ids, the new one gets its own, the row count stays one.
	updated := strings.Replace(buildSpec, `"Run the tests."`, `"Run the tests and lint."`, 1)
	updated = strings.Replace(updated, `"on_success":"end","on_failure":"exec"}],`,
		`"on_success":"ship","on_failure":"exec"},
  {"name":"ship","agent":"coder","prompt":"Commit.","gate":false,"gate_pass":"","gate_fail":"","pause_before":true,"compact_before":false,"on_success":"","on_failure":""}],`, 1)
	out = invokeText(t, save, updated)
	if !strings.HasPrefix(out, `Updated workflow "build" (3 → 4 steps)`) {
		t.Fatalf("update said %q", out)
	}
	list, _ = runner.Deps.Workflows.List(ctx)
	if len(list) != 1 || list[0].ID != stored.ID || len(list[0].Steps) != 4 {
		t.Fatalf("after update: %d workflows, id %q, %d steps", len(list), list[0].ID, len(list[0].Steps))
	}
	now := list[0].Steps
	if now[0].ID != plan.ID || now[1].ID != exec.ID || now[2].ID != verify.ID {
		t.Fatalf("kept steps changed ids: %+v vs %+v", now, stored.Steps)
	}
	if now[3].ID == "" || now[3].ID == verify.ID || !now[3].PauseBefore || now[2].OnSuccess != now[3].ID || now[2].Prompt != "Run the tests and lint." {
		t.Fatalf("new step / edge: %+v", now)
	}
}

// A save that would not land is refused to the model as text, writes nothing,
// and — the gate's half — needs no approval, so a person never reviews it.
func TestSaveWorkflowRefusesWhatWouldNotSave(t *testing.T) {
	ctx := context.Background()
	_, srv := newRecordingModel(t, func(int, []byte) []any { return sayOutput("ok") })
	runner, _, _ := workflowToolsFixture(t, srv.URL)
	save := toolNamed(t, runner.workflowTools(ctx, store.LocalUserID), WorkflowSaveToolName)

	cases := []struct{ name, spec, want string }{
		{"unknown agent", strings.Replace(buildSpec, `"agent":"reviewer"`, `"agent":"tester"`, 1), `no agent named "tester" (available: coder, planner, reviewer)`},
		{"duplicate step name", strings.Replace(buildSpec, `"name":"exec"`, `"name":"Plan"`, 1), `duplicate step name "Plan"`},
		{"end as a step name", strings.Replace(buildSpec, `"name":"exec"`, `"name":"end"`, 1), `"end" is reserved`},
		{"edge to nowhere", strings.Replace(buildSpec, `"on_failure":"exec"`, `"on_failure":"fix"`, 1), `on_failure names "fix"`},
		{"no steps", `{"name":"x","description":"d","steps":[],"budget":{"max_steps":0,"max_tokens":0,"max_minutes":0,"max_laps":0}}`, "at least one step"},
		{"no description", strings.Replace(buildSpec, `"description":"Implement a feature end to end, with tests"`, `"description":""`, 1), "description is required"},
		{"gate words without a gate", strings.Replace(buildSpec, `"gate":false,"gate_pass":""`, `"gate":false,"gate_pass":"OK"`, 1), "gate_pass / gate_fail need gate true"},
		{"same gate words", strings.Replace(buildSpec, `"gate":true,"gate_pass":"","gate_fail":""`, `"gate":true,"gate_pass":"ok","gate_fail":"OK"`, 1), "pass and fail words must differ"},
	}
	for _, tc := range cases {
		out := invokeText(t, save, tc.spec)
		if !strings.HasPrefix(out, "Nothing was saved: ") || !strings.Contains(out, tc.want) {
			t.Errorf("%s: said %q, want a refusal mentioning %q", tc.name, out, tc.want)
		}
		needs, err := save.NeedsApprovalFunc(ctx, nil, tc.spec, "call")
		if err != nil || needs {
			t.Errorf("%s: NeedsApproval = %v (%v), want false — nobody reviews a save that cannot land", tc.name, needs, err)
		}
	}
	if list, _ := runner.Deps.Workflows.List(ctx); len(list) != 0 {
		t.Fatalf("refusals wrote %d workflows", len(list))
	}
	if needs, err := save.NeedsApprovalFunc(ctx, nil, buildSpec, "call"); err != nil || !needs {
		t.Fatalf("a valid save: NeedsApproval = %v (%v), want true", needs, err)
	}
	// Malformed arguments are not a write either.
	if needs, _ := save.NeedsApprovalFunc(ctx, nil, `{"name":`, "call"); needs {
		t.Fatal("malformed arguments must not reach a person")
	}
}

// get_workflow returns the definition in the save shape: a nameless step is
// called by its position, edges and agents by name, and what it returns saves
// back as the same sequence.
func TestGetWorkflowRoundTrips(t *testing.T) {
	ctx := context.Background()
	_, srv := newRecordingModel(t, func(int, []byte) []any { return sayOutput("ok") })
	runner, _, coder := workflowToolsFixture(t, srv.URL)
	reviewer, _ := runner.agentConfigByName(ctx, "", "reviewer")
	tools := runner.workflowTools(ctx, store.LocalUserID)
	get := toolNamed(t, tools, WorkflowGetToolName)

	if out := invokeText(t, get, `{"name":"build"}`); !strings.Contains(out, "no workflows yet") {
		t.Fatalf("empty server said %q", out)
	}
	// Hub-style: nameless steps, edges by id, a gate with its own words.
	review := store.WorkflowStep{ID: "s-review", AgentConfigID: reviewer.ID, Prompt: "Review.", Gate: &store.StepGate{Pass: "LGTM", Fail: "NOPE"}}
	fix := store.WorkflowStep{ID: "s-fix", Name: "fix", AgentConfigID: coder.ID, Prompt: "Fix.", OnSuccess: "s-review"}
	review.OnSuccess, review.OnFailure = store.WorkflowStepEnd, "s-fix"
	wf := &store.Workflow{Name: "review", Description: "Review then fix", Steps: store.WorkflowSteps{review, fix}, Budget: store.WorkflowBudget{MaxLaps: 5}}
	if err := store.NormalizeWorkflow(wf); err != nil {
		t.Fatal(err)
	}
	if err := runner.Deps.Workflows.Create(ctx, wf); err != nil {
		t.Fatal(err)
	}

	out := invokeText(t, get, `{"name":"Review"}`)
	var spec workflowSpec
	if err := json.Unmarshal([]byte(out), &spec); err != nil {
		t.Fatalf("get returned %q: %v", out, err)
	}
	if spec.Name != "review" || len(spec.Steps) != 2 || spec.Budget.MaxLaps != 5 {
		t.Fatalf("spec = %+v", spec)
	}
	first, second := spec.Steps[0], spec.Steps[1]
	if first.Name != "Step 1" || first.Agent != "reviewer" || !first.Gate || first.GatePass != "LGTM" || first.OnSuccess != "end" || first.OnFailure != "fix" {
		t.Fatalf("first step = %+v", first)
	}
	if second.Name != "fix" || second.Agent != "coder" || second.OnSuccess != "Step 1" {
		t.Fatalf("second step = %+v", second)
	}
	if out := invokeText(t, get, `{"name":"nope"}`); !strings.Contains(out, `No workflow named "nope". Available: review`) {
		t.Fatalf("unknown name said %q", out)
	}
	if out := invokeText(t, get, `{"name":""}`); !strings.Contains(out, "Available: review") {
		t.Fatalf("empty name said %q", out)
	}

	// Saving the read back keeps the ids of both steps: the same sequence,
	// now with the nameless step named as it was read.
	spec.Description = "Review, then fix what it finds"
	back, _ := json.Marshal(spec)
	if out := invokeText(t, toolNamed(t, tools, WorkflowSaveToolName), string(back)); !strings.HasPrefix(out, `Updated workflow "review" (2 → 2 steps)`) {
		t.Fatalf("round-trip save said %q", out)
	}
	list, _ := runner.Deps.Workflows.List(ctx)
	if len(list) != 1 || list[0].Steps[0].ID != "s-review" || list[0].Steps[1].ID != "s-fix" || list[0].Steps[0].Name != "Step 1" || list[0].Steps[1].OnSuccess != "s-review" {
		t.Fatalf("after round trip: %+v", list[0].Steps)
	}
}

// End to end: the model's save pauses the run on an approval, the decision
// resumes it — the rebuilt agent carries the tool — and only then is the
// definition written; an invalid save never pauses at all.
func TestSaveWorkflowIsApprovedThenWritten(t *testing.T) {
	ctx := context.Background()
	model, srv := newRecordingModel(t, func(call int, _ []byte) []any {
		switch call {
		case 1:
			return callOutput("call_1", WorkflowSaveToolName, buildSpec)
		case 3: // the second conversation: an invalid save
			return callOutput("call_2", WorkflowSaveToolName, strings.Replace(buildSpec, `"agent":"reviewer"`, `"agent":"tester"`, 1))
		}
		return sayOutput("saved")
	})
	runner, sess, coder := workflowToolsFixture(t, srv.URL)

	first := runChat(t, runner, sess, coder, "save the build workflow")
	if !first.Interrupted || len(first.Interruptions) != 1 || first.Interruptions[0].CallID != "call_1" {
		t.Fatalf("outcome = %+v, want a pause on call_1", first)
	}
	if list, _ := runner.Deps.Workflows.List(ctx); len(list) != 0 {
		t.Fatalf("written before approval: %d workflows", len(list))
	}
	if pending, _, err := runner.Deps.PendingApprovals.FindByToolCall(ctx, "call_1"); err != nil || pending.RunID != first.RunID {
		t.Fatalf("pending approval = %+v (%v), want one on the run", pending, err)
	}

	resumed := make(chan *RunOutcome, 1)
	runID, _, err := runner.ResolveApproval(ctx, "call_1", true, ApprovalOnce, "", func(o *RunOutcome) { resumed <- o })
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if runID != first.RunID {
		t.Fatalf("approval resumed %q, want %q", runID, first.RunID)
	}
	select {
	case o := <-resumed:
		if o.Interrupted || o.ErrMessage != "" || o.FinalText != "saved" {
			t.Fatalf("resumed outcome = %+v", o)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the resumed run never finished")
	}
	list, _ := runner.Deps.Workflows.List(ctx)
	if len(list) != 1 || list[0].Name != "build" || len(list[0].Steps) != 3 {
		t.Fatalf("after approval: %+v", list)
	}
	if !strings.Contains(model.bodyOfCall(2), `Created workflow \"build\"`) {
		t.Fatalf("the model was not told the outcome: %s", model.bodyOfCall(2))
	}

	// The invalid save: no pause, the refusal goes straight back to the model.
	other := &store.Session{OwnerID: store.LocalUserID, ID: store.NewID(), Name: "chat 2"}
	if err := runner.Deps.Sessions.Create(ctx, other); err != nil {
		t.Fatal(err)
	}
	second := runChat(t, runner, other, coder, "save a broken workflow")
	if second.Interrupted || second.ErrMessage != "" {
		t.Fatalf("invalid save outcome = %+v, want no pause and no error", second)
	}
	if !strings.Contains(model.bodyOfCall(4), `Nothing was saved: step 3 (verify): no agent named`) {
		t.Fatalf("the model was not told why: %s", model.bodyOfCall(4))
	}
	if list, _ := runner.Deps.Workflows.List(ctx); len(list) != 1 {
		t.Fatalf("the invalid save wrote: %d workflows", len(list))
	}
}

// Everyone carries save_workflow now: a member's save lands in their own
// private set, and editing a GLOBAL definition through the tool stays the
// admin's act (spec §5.29).
func TestSaveWorkflowScopes(t *testing.T) {
	ctx := context.Background()
	model, srv := newRecordingModel(t, func(int, []byte) []any { return sayOutput("ok") })
	runner, _, coder := workflowToolsFixture(t, srv.URL)
	users := store.NewUserStore(runner.db)
	runner.Deps.Users = users
	if _, err := users.EnsureLocalUser(ctx); err != nil {
		t.Fatal(err)
	}
	member := &store.User{ID: store.NewID(), Email: "m@example.com", Role: store.RoleMember}
	if _, err := runner.db.NewInsert().Model(member).Exec(ctx); err != nil {
		t.Fatal(err)
	}

	theirs := &store.Session{OwnerID: member.ID, ID: store.NewID(), Name: "member chat"}
	if err := runner.Deps.Sessions.Create(ctx, theirs); err != nil {
		t.Fatal(err)
	}
	runChat(t, runner, theirs, coder, "member asks")
	memberTools := model.toolsOfBody(t, "member asks", time.Second)
	if !slices.Contains(memberTools, WorkflowSaveToolName) || !slices.Contains(memberTools, WorkflowGetToolName) {
		t.Fatalf("member run tools = %v, want both workflow tools", memberTools)
	}

	// A member's save creates a PRIVATE definition owned by them.
	spec := workflowSpec{Name: "member-flow", Description: "when the member asks",
		Steps: []workflowStep{{Name: "one", Agent: "coder", Prompt: "do it"}}}
	if _, err := runner.saveWorkflow(ctx, member.ID, spec); err != nil {
		t.Fatalf("member save: %v", err)
	}
	list, err := runner.Deps.Workflows.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var saved *store.Workflow
	for i := range list {
		if list[i].Name == "member-flow" {
			saved = &list[i]
		}
	}
	if saved == nil || saved.Scope != store.ScopePrivate || saved.OwnerID != member.ID {
		t.Fatalf("member save landed as %+v, want private owned by the member", saved)
	}

	// A GLOBAL definition refuses the member's edit through the tool.
	global := &store.Workflow{Name: "team-flow", Description: "shared", Scope: store.ScopeGlobal,
		Steps: store.WorkflowSteps{{ID: store.NewID(), Name: "one", AgentConfigID: saved.Steps[0].AgentConfigID, Prompt: "p"}}}
	if err := runner.Deps.Workflows.Create(ctx, global); err != nil {
		t.Fatal(err)
	}
	res, err := runner.saveWorkflow(ctx, member.ID, workflowSpec{Name: "team-flow", Description: "hijack",
		Steps: []workflowStep{{Name: "one", Agent: "coder", Prompt: "changed"}}})
	if err != nil {
		t.Fatalf("member save over global: %v", err)
	}
	txt := ""
	if len(res.Content) > 0 {
		if tt, ok := res.Content[0].(agents.ToolOutputText); ok {
			txt = tt.Text
		}
	}
	if !strings.Contains(txt, "only an admin") {
		t.Fatalf("member editing a global workflow = %q, want the admin-only refusal", txt)
	}
	if got, _ := runner.Deps.Workflows.Get(ctx, global.ID); got.Description != "shared" {
		t.Fatalf("the refused save changed the global row: %+v", got)
	}
}
