package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzir/agents-go/agents/tasks"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// finishedResponse is one completed assistant message, the shape both the
// streaming and non-streaming replies carry.
func finishedResponse() map[string]any {
	return map[string]any{
		"id": "resp_1", "object": "response", "created_at": 0, "status": "completed", "model": "gpt-test",
		"output": []any{map[string]any{
			"type": "message", "id": "msg_1", "status": "completed", "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": "done", "annotations": []any{}}},
		}},
		"usage": map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2},
	}
}

// oneShotModel answers every call with a finished assistant message, so each
// workflow step is one quick turn. It answers NON-streaming requests as JSON:
// compaction summarizes through an ordinary call, and a server that only
// speaks SSE fails it in a way that reads as a compaction bug.
func oneShotModel(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Stream bool `json:"stream"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if !body.Stream {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(finishedResponse())
			return
		}
		send := sseWriter(w)
		sseCreated(send)
		send("response.completed", map[string]any{
			"type": "response.completed", "sequence_number": 1,
			"response": finishedResponse(),
		})
	}))
}

// workflowFixture wires a runner with a session and a three-step workflow whose
// steps use three different agents — the point of a workflow.
func workflowFixture(t *testing.T, modelURL string) (*Runner, *store.Session, *store.Workflow) {
	t.Helper()
	ctx := context.Background()
	runner, sessions, _, agentConfigs := newTaskTestRunner(t)
	runner.Deps.Workflows = store.NewWorkflowStore(runner.db)
	providerID := testProvider(t, runner.db, "endpoint", "k", modelURL)

	var steps store.WorkflowSteps
	for _, name := range []string{"plan", "exec", "verify"} {
		ac := &store.AgentConfig{Name: name, Model: "gpt-test", ProviderID: providerID}
		if err := agentConfigs.Create(ctx, ac); err != nil {
			t.Fatal(err)
		}
		steps = append(steps, store.WorkflowStep{Name: name, AgentConfigID: ac.ID, Prompt: "do the " + name})
	}
	wf := &store.Workflow{Name: "review", Description: "review the work", Steps: steps}
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
	return runner, sess, wf
}

// awaitWorkflow waits for the execution's task to leave the working states,
// and returns the row with its state decoded.
func awaitWorkflow(t *testing.T, runner *Runner, taskID string, within time.Duration) (*store.Task, *store.WorkflowState) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		row, err := runner.Deps.Tasks.Get(context.Background(), taskID)
		if err != nil {
			t.Fatal(err)
		}
		if isTerminalTaskStatus(row.Status) || time.Now().After(deadline) {
			st, err := store.DecodeWorkflowState(row.State)
			if err != nil {
				t.Fatalf("task %s carries no workflow state: %v", taskID, err)
			}
			return row, st
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The whole sequence runs on ONE session, in order, each step under its own
// agent — and the execution ends completed with the last step recorded.
func TestWorkflowRunsEveryStepInOrder(t *testing.T) {
	ctx := context.Background()
	srv := oneShotModel(t)
	defer srv.Close()
	runner, sess, wf := workflowFixture(t, srv.URL)

	info, err := runner.StartWorkflow(ctx, wf.ID, sess.ID, "sort a list", "call_start")
	if err != nil {
		t.Fatalf("StartWorkflow: %v", err)
	}
	if info.Kind != store.TaskKindWorkflow || info.Label != wf.Name {
		t.Fatalf("started %+v, want a task of the workflow kind named after it", info)
	}

	done, st := awaitWorkflow(t, runner, info.TaskID, 15*time.Second)
	if done.Status != "completed" {
		t.Fatalf("status = %q (%s), want completed", done.Status, done.Summary)
	}
	if st.StepID != wf.Steps[2].ID {
		t.Fatalf("ended at step %q, want the last one", st.StepID)
	}
	// The card that started it follows the execution: the row names the call.
	if done.ToolCallID != "call_start" {
		t.Fatalf("tool call = %q, want the start_workflow call", done.ToolCallID)
	}
	// The launch log keeps every step, not just the current one — it is what
	// maps a finished sequence's turns back to the step that produced them.
	if len(st.StepRuns) != 3 {
		t.Fatalf("step runs = %+v, want one per step", st.StepRuns)
	}
	for i, sr := range st.StepRuns {
		if sr.StepID != wf.Steps[i].ID || sr.RunID == "" {
			t.Fatalf("step run %d = %+v, want step %q with a run", i, sr, wf.Steps[i].ID)
		}
	}
	// The last launched run is the row's current one — the same id, so a
	// reader can join the log to the task.
	if st.StepRuns[2].RunID != done.RunID {
		t.Fatalf("last logged run %q, row run %q — want the same", st.StepRuns[2].RunID, done.RunID)
	}
	// The ending is written too: the last step's outcome is in the row's
	// state, not only derivable from the task's status.
	for i, sr := range st.StepRuns {
		if sr.Outcome != store.StepOutcomeCompleted {
			t.Fatalf("step run %d outcome = %q, want completed", i, sr.Outcome)
		}
	}

	// Every step's turn landed in the execution's own CHILD session — never in
	// the conversation that asked — and they share it, so later steps read
	// what earlier ones did.
	ref, err := store.RefFor(ctx, runner.db, done.ChildSessionID)
	if err != nil {
		t.Fatal(err)
	}
	views, err := store.NewEntryStoreFor(runner.db, ref).GetEntries(ctx, ref, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var prompts []string
	for _, v := range views {
		if v.Role == "user" {
			prompts = append(prompts, v.Content)
		}
	}
	if len(prompts) != 3 {
		t.Fatalf("session carries %d user turns, want one per step", len(prompts))
	}
	// The typed task leads the first step and is NOT repeated afterwards — by
	// then it is in the transcript the later steps read.
	if !strings.HasPrefix(prompts[0], "sort a list\n\n") || !strings.HasSuffix(prompts[0], wf.Steps[0].Prompt) {
		t.Fatalf("first turn = %q, want the input leading the step prompt", prompts[0])
	}
	if strings.Contains(prompts[1], "sort a list") {
		t.Fatalf("second turn = %q, want the step prompt alone", prompts[1])
	}
	// And the parent's own transcript stayed out of it.
	parentRef, err := store.RefFor(ctx, runner.db, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	parentRows, err := store.NewEntryStoreFor(runner.db, parentRef).GetEntries(ctx, parentRef, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range parentRows {
		if v.Role == "user" && strings.Contains(v.Content, wf.Steps[0].Prompt) {
			t.Fatalf("a step's prompt reached the parent transcript: %q", v.Content)
		}
	}
}

// A step that fails ends the sequence THERE: the later steps never run, and the
// state keeps the failed step so a retry resumes from it.
func TestWorkflowStopsAtAFailedStep(t *testing.T) {
	ctx := context.Background()
	// No model server: the first step's call fails.
	runner, sess, wf := workflowFixture(t, "http://127.0.0.1:1")

	info, err := runner.StartWorkflow(ctx, wf.ID, sess.ID, "", "")
	if err != nil {
		t.Fatalf("StartWorkflow: %v", err)
	}
	done, st := awaitWorkflow(t, runner, info.TaskID, 20*time.Second)
	if done.Status != "failed" {
		t.Fatalf("status = %q, want failed", done.Status)
	}
	if st.StepID != wf.Steps[0].ID {
		t.Fatalf("failed at step %q, want the first one — a retry resumes from here", st.StepID)
	}
	if done.Summary == "" {
		t.Error("a failed execution should carry the reason")
	}
}

// The execution records the turn that asked for it, so its result nests under
// that turn in the trace instead of opening a second root. The id comes from
// the run context the tool call executes in — nothing else knows it.
func TestWorkflowRecordsTheTurnThatStartedIt(t *testing.T) {
	ctx := context.Background()
	srv := oneShotModel(t)
	defer srv.Close()
	runner, sess, wf := workflowFixture(t, srv.URL)

	info, err := runner.StartWorkflow(tasks.WithParentRunID(ctx, "run_asked"), wf.ID, sess.ID, "", "")
	if err != nil {
		t.Fatalf("StartWorkflow: %v", err)
	}
	row, err := runner.Deps.Tasks.Get(ctx, info.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if row.ParentRunID != "run_asked" {
		t.Fatalf("parent run = %q, want the run whose tool call started it", row.ParentRunID)
	}
	if row.Kind != store.TaskKindWorkflow || row.ParentSessionID != sess.ID {
		t.Fatalf("row = %+v, want a workflow-kind task of the asking session", row)
	}
}

// flakyModel fails the first n calls with a hard 400 (nothing retries past it)
// and answers normally afterwards, so a workflow can be watched taking its
// failure edge and then succeeding.
func flakyModel(t *testing.T, n int) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	calls := 0
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		fail := calls <= n
		mu.Unlock()
		if fail {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"the step blew up"}}`))
			return
		}
		var body struct {
			Stream bool `json:"stream"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if !body.Stream {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(finishedResponse())
			return
		}
		send := sseWriter(w)
		sseCreated(send)
		send("response.completed", map[string]any{
			"type": "response.completed", "sequence_number": 1, "response": finishedResponse(),
		})
	}))
}

// A failed step takes its on_failure edge instead of ending the execution, and
// the handler's on_success can point BACKWARDS — which is the whole of looping.
// The handler is also told what it is handling: a failed run leaves no usable
// account of the failure in the transcript.
func TestWorkflowFailureEdgeLoopsBackAndCompletes(t *testing.T) {
	ctx := context.Background()
	srv := flakyModel(t, 1) // only the first step run fails
	defer srv.Close()
	runner, sess, wf := workflowFixture(t, srv.URL)

	// work → (fail) → recover → (ok) → work → (ok) → end
	wf.Steps = store.WorkflowSteps{
		{ID: "work", Name: "work", AgentConfigID: wf.Steps[0].AgentConfigID, Prompt: "do the work",
			OnFailure: "recover", OnSuccess: store.WorkflowStepEnd},
		{ID: "recover", Name: "recover", AgentConfigID: wf.Steps[1].AgentConfigID, Prompt: "clean up",
			OnSuccess: "work"},
	}
	if err := runner.Deps.Workflows.Update(ctx, wf.ID, wf); err != nil {
		t.Fatal(err)
	}

	info, err := runner.StartWorkflow(ctx, wf.ID, sess.ID, "", "")
	if err != nil {
		t.Fatalf("StartWorkflow: %v", err)
	}
	done, st := awaitWorkflow(t, runner, info.TaskID, 20*time.Second)
	if done.Status != "completed" {
		t.Fatalf("status = %q (%s), want completed — the failure edge should have carried it", done.Status, done.Summary)
	}
	// work, recover, work: the log is what proves a step ran twice.
	var visited []string
	for _, sr := range st.StepRuns {
		visited = append(visited, sr.StepID)
	}
	if strings.Join(visited, ",") != "work,recover,work" {
		t.Fatalf("visited %v, want work → recover → work", visited)
	}

	// The handler was told what it was handling.
	ref, err := store.RefFor(ctx, runner.db, done.ChildSessionID)
	if err != nil {
		t.Fatal(err)
	}
	views, err := store.NewEntryStoreFor(runner.db, ref).GetEntries(ctx, ref, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var handled bool
	for _, v := range views {
		if v.Role == "user" && strings.HasPrefix(v.Content, "The previous step failed:") && strings.Contains(v.Content, "clean up") {
			handled = true
		}
	}
	if !handled {
		t.Fatal("the handler step's turn must be led by the failure it is handling")
	}
}

// An on_failure edge can point backwards, so something has to stop a workflow
// that never succeeds. The ceiling counts step LAUNCHES, because that is what
// costs — and the driver refuses to continue past it, which fails the task.
func TestWorkflowStopsAtTheStepCeiling(t *testing.T) {
	// The lap bound raised to the ceiling, so the ceiling is what stops this
	// self-loop.
	st := &store.WorkflowState{
		Steps:  store.WorkflowSteps{{ID: "a", AgentConfigID: "x", Prompt: "p", OnSuccess: "a"}},
		StepID: "a",
		Budget: store.WorkflowBudget{MaxLaps: store.MaxStepRuns},
	}
	for range store.MaxStepRuns {
		st.StepRuns = st.StepRuns.With("a", store.NewID())
	}
	last := st.StepRuns[len(st.StepRuns)-1].RunID
	cont, err := continueWorkflow(st, last, tasks.RunOutcome{Status: tasks.StatusCompleted, Text: "another lap"}, 0)
	if cont != nil || err == nil || !strings.Contains(err.Error(), "looping") {
		t.Fatalf("continue = %+v, %v — want a refusal for looping", cont, err)
	}
	// One launch short of the ceiling, the loop turns once more.
	st.StepRuns = st.StepRuns[1:]
	if cont, err := continueWorkflow(st, last, tasks.RunOutcome{Status: tasks.StatusCompleted}, 0); err != nil || cont == nil {
		t.Fatalf("continue = %+v, %v — want one more lap", cont, err)
	}
}

// Retrying an execution the ceiling stopped is refused at the launch, before
// a step runs: one more lap could only end the same way, at that lap's price.
func TestWorkflowRetryPastTheCeilingIsRefusedBeforeARun(t *testing.T) {
	ctx := context.Background()
	srv := oneShotModel(t)
	defer srv.Close()
	runner, sess, wf := workflowFixture(t, srv.URL)
	st := &store.WorkflowState{WorkflowID: wf.ID, Steps: wf.Steps, StepID: wf.Steps[0].ID}
	for range store.MaxStepRuns {
		st.StepRuns = st.StepRuns.With(wf.Steps[0].ID, store.NewID())
	}
	err := runner.launchWorkflowStep(ctx, tasks.LaunchRequest{
		TaskID: "t", RunID: store.NewID(), Kind: store.TaskKindWorkflow, State: st.Encode(), SessionID: sess.ID,
	})
	if err == nil || !strings.Contains(err.Error(), "looping") {
		t.Fatalf("launch past the ceiling = %v — want the ceiling's refusal", err)
	}
	// Nothing ran: the session the step would have run on is still empty.
	ref, err := store.RefFor(ctx, runner.db, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	views, err := store.NewEntryStoreFor(runner.db, ref).GetEntries(ctx, ref, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 0 {
		t.Fatalf("%d entries on the step's session — the refusal must come before a run", len(views))
	}
}

// A definition's budget bounds each execution: steps and minutes read off the
// launch log, tokens off the session — checked before a step is launched, so
// a run in flight is never cut, and a pause on a person costs no minutes.
func TestWorkflowBudgetStopsBeforeTheNextStep(t *testing.T) {
	loop := func(b store.WorkflowBudget, log store.StepRuns) *store.WorkflowState {
		return &store.WorkflowState{
			Steps:    store.WorkflowSteps{{ID: "a", AgentConfigID: "x", Prompt: "p", OnSuccess: "a"}},
			Budget:   b,
			StepID:   "a",
			StepRuns: log,
		}
	}
	two := store.StepRuns{{StepID: "a", RunID: "r1", Outcome: "completed"}, {StepID: "a", RunID: "r2"}}
	done := tasks.RunOutcome{Status: tasks.StatusCompleted}

	if _, err := continueWorkflow(loop(store.WorkflowBudget{MaxSteps: 2}, two), "r2", done, 0); err == nil || !strings.Contains(err.Error(), "2 of 2 steps") {
		t.Fatalf("steps: err = %v, want the budget's refusal", err)
	}
	if cont, err := continueWorkflow(loop(store.WorkflowBudget{MaxSteps: 3}, two), "r2", done, 0); err != nil || cont == nil {
		t.Fatalf("steps under budget: cont=%v err=%v, want another step", cont, err)
	}
	if _, err := continueWorkflow(loop(store.WorkflowBudget{MaxTokens: 100}, two), "r2", done, 150); err == nil || !strings.Contains(err.Error(), "150 of 100 tokens") {
		t.Fatalf("tokens: err = %v, want the budget's refusal", err)
	}
	// Two runs of 40s an hour apart (the current one ends now, as the driver
	// stamps it): 80s of run time, not an hour.
	now := time.Now()
	timed := store.StepRuns{
		{StepID: "a", RunID: "r1", Outcome: "completed", StartedAt: now.Add(-time.Hour - 40*time.Second), EndedAt: now.Add(-time.Hour)},
		{StepID: "a", RunID: "r2", StartedAt: now.Add(-40 * time.Second)},
	}
	if cont, err := continueWorkflow(loop(store.WorkflowBudget{MaxMinutes: 2}, timed), "r2", done, 0); err != nil || cont == nil {
		t.Fatalf("minutes under budget: cont=%v err=%v — a pause must not count", cont, err)
	}
	if _, err := continueWorkflow(loop(store.WorkflowBudget{MaxMinutes: 1}, timed), "r2", done, 0); err == nil || !strings.Contains(err.Error(), "of 1 minutes") {
		t.Fatalf("minutes: err = %v, want the budget's refusal", err)
	}
	// No budget bounds nothing.
	if cont, err := continueWorkflow(loop(store.WorkflowBudget{}, two), "r2", done, 1<<30); err != nil || cont == nil {
		t.Fatalf("no budget: cont=%v err=%v", cont, err)
	}
}

// End to end, the token budget is measured on the execution's own session:
// the fake model prices every call at two tokens, so a three-token budget lets
// the first step through and stops the sequence after the second — and a
// retry is refused before it runs anything.
func TestWorkflowTokenBudgetIsMeasuredOnTheSession(t *testing.T) {
	ctx := context.Background()
	srv := oneShotModel(t)
	defer srv.Close()
	runner, sess, wf := workflowFixture(t, srv.URL)
	wf.Budget = store.WorkflowBudget{MaxTokens: 3}
	if err := runner.Deps.Workflows.Update(ctx, wf.ID, wf); err != nil {
		t.Fatal(err)
	}
	info, err := runner.StartWorkflow(ctx, wf.ID, sess.ID, "spend wisely", "")
	if err != nil {
		t.Fatal(err)
	}
	done, st := awaitWorkflow(t, runner, info.TaskID, 15*time.Second)
	if done.Status != "failed" || !strings.Contains(done.Summary, "budget exhausted") || !strings.Contains(done.Summary, "tokens") {
		t.Fatalf("status = %q (%s), want failed by the token budget", done.Status, done.Summary)
	}
	if st.StepID != wf.Steps[1].ID || len(st.StepRuns) != 2 {
		t.Fatalf("stopped at %q after %d launches, want the second step, two launches", st.StepID, len(st.StepRuns))
	}
	if st.Budget.MaxTokens != 3 || st.Stopped != store.StoppedByBudget {
		t.Fatalf("state budget = %+v stopped %q, want the definition's snapshotted and the bound named", st.Budget, st.Stopped)
	}
	if _, err := runner.RetryTask(info.TaskID); err == nil || !strings.Contains(err.Error(), "budget exhausted") {
		t.Fatalf("retry past the budget = %v, want the budget's refusal before a run", err)
	}
}

// Every other kind of task ends with its run: the hook is a no-op for it.
func TestContinueTaskLeavesPlainTasksAlone(t *testing.T) {
	runner, _, _, _ := newTaskTestRunner(t)
	cont, err := runner.continueTask(context.Background(), &tasks.Task{ID: "t"}, tasks.RunOutcome{Status: tasks.StatusCompleted})
	if cont != nil || err != nil {
		t.Fatalf("continue = %+v, %v — want nothing for a plain task", cont, err)
	}
}

// Background work is budgeted, not forbidden: an execution is a task, so what
// bounds it is the per-session cap every task answers to.
func TestWorkflowRefusesPastTheBackgroundBudget(t *testing.T) {
	ctx := context.Background()
	srv := oneShotModel(t)
	defer srv.Close()
	runner, sess, wf := workflowFixture(t, srv.URL)

	for range runner.hub.maxTasks {
		if err := runner.Deps.Tasks.Create(ctx, &store.Task{
			ID: store.NewID(), RunID: store.NewID(), ParentSessionID: sess.ID, ChildSessionID: store.NewID(),
			Label: "other", Status: "working",
		}); err != nil {
			t.Fatal(err)
		}
	}
	_, err := runner.StartWorkflow(ctx, wf.ID, sess.ID, "", "")
	if err == nil {
		t.Fatal("a workflow past the background budget must be refused")
	}
	if !errors.As(err, new(tasks.ErrTaskLimit)) {
		t.Fatalf("err = %v, want the task cap's refusal", err)
	}
}

// The definition may be edited or deleted while an execution is in flight: the
// task carries its own snapshot and keeps running what it started with.
func TestWorkflowRunCarriesItsOwnSnapshot(t *testing.T) {
	ctx := context.Background()
	srv := oneShotModel(t)
	defer srv.Close()
	runner, sess, wf := workflowFixture(t, srv.URL)

	info, err := runner.StartWorkflow(ctx, wf.ID, sess.ID, "", "")
	if err != nil {
		t.Fatalf("StartWorkflow: %v", err)
	}
	if err := runner.Deps.Workflows.Delete(ctx, wf.ID); err != nil {
		t.Fatal(err)
	}
	done, st := awaitWorkflow(t, runner, info.TaskID, 15*time.Second)
	if done.Status != "completed" {
		t.Fatalf("status = %q (%s), want completed despite the deleted definition", done.Status, done.Summary)
	}
	if len(st.Steps) != 3 {
		t.Fatalf("the snapshot should still hold every step, got %d", len(st.Steps))
	}
}

// A restart leaves no live step, so an execution recorded as running is failed
// by the task sweep at the step it reached — which a retry resumes from.
func TestWorkflowInterruptedByRestartFailsAtItsStep(t *testing.T) {
	ctx := context.Background()
	runner, sess, wf := workflowFixture(t, "http://127.0.0.1:1")

	st := &store.WorkflowState{Steps: wf.Steps, StepID: wf.Steps[1].ID}
	row := &store.Task{
		ID: store.NewID(), RunID: "run_gone", Kind: store.TaskKindWorkflow, State: st.Encode(),
		ParentSessionID: sess.ID, ChildSessionID: store.NewID(), Label: wf.Name, Status: "working",
	}
	if err := runner.Deps.Tasks.Create(ctx, row); err != nil {
		t.Fatal(err)
	}
	runner.FailOrphanedTasks(ctx)

	after, err := runner.Deps.Tasks.Get(ctx, row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != "failed" {
		t.Fatalf("status = %q, want failed", after.Status)
	}
	got, err := store.DecodeWorkflowState(after.State)
	if err != nil {
		t.Fatal(err)
	}
	if got.StepID != wf.Steps[1].ID {
		t.Fatalf("the reached step must survive for a retry, got %q", got.StepID)
	}
}

// NormalizeWorkflow fills a stable id for every step: positions shift when one
// is inserted, and a run in flight, a retry and the record of what happened all
// keep naming the same step.
func TestNormalizeWorkflowFillsStableStepIDs(t *testing.T) {
	wf := &store.Workflow{Name: "w", Description: "d", Steps: store.WorkflowSteps{
		{AgentConfigID: "a1", Prompt: "one"},
		{ID: "kept", AgentConfigID: "a2", Prompt: "two"},
	}}
	if err := store.NormalizeWorkflow(wf); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if wf.Steps[0].ID == "" || wf.Steps[1].ID != "kept" {
		t.Fatalf("ids wrong: %+v", wf.Steps)
	}
	// A duplicate id would make "retry from here" ambiguous.
	dup := &store.Workflow{Name: "w", Description: "d", Steps: store.WorkflowSteps{
		{ID: "same", AgentConfigID: "a1", Prompt: "one"},
		{ID: "same", AgentConfigID: "a2", Prompt: "two"},
	}}
	if err := store.NormalizeWorkflow(dup); err == nil {
		t.Fatal("duplicate step ids must be refused")
	}
	// A step with no prompt has nothing to run.
	empty := &store.Workflow{Name: "w", Steps: store.WorkflowSteps{{AgentConfigID: "a1"}}}
	if err := store.NormalizeWorkflow(empty); err == nil {
		t.Fatal("a step without a prompt must be refused")
	}
}

// A step marked compact_before folds the conversation first. The pass runs
// from the launcher, i.e. inside the previous run's TEARDOWN — after the hub
// released the session, or the busy check would refuse it and the flag would
// quietly do nothing.
func TestWorkflowCompactsBeforeAStep(t *testing.T) {
	ctx := context.Background()
	srv := oneShotModel(t)
	defer srv.Close()
	runner, sessions, _, agentConfigs := newTaskTestRunner(t)
	runner.Deps.Workflows = store.NewWorkflowStore(runner.db)
	providerID := testProvider(t, runner.db, "endpoint", "k", srv.URL)

	ac := &store.AgentConfig{Name: "worker", Model: "gpt-test", ProviderID: providerID}
	ac.Compaction = store.CompactionGroup{Enabled: true, Threshold: 1, Window: 1}
	if err := agentConfigs.Create(ctx, ac); err != nil {
		t.Fatal(err)
	}
	wf := &store.Workflow{Name: "folding", Description: "fold between steps", Steps: store.WorkflowSteps{
		{Name: "one", AgentConfigID: ac.ID, Prompt: "first"},
		{Name: "two", AgentConfigID: ac.ID, Prompt: "second", CompactBefore: true},
	}}
	if err := store.NormalizeWorkflow(wf); err != nil {
		t.Fatal(err)
	}
	if err := runner.Deps.Workflows.Create(ctx, wf); err != nil {
		t.Fatal(err)
	}
	sess := &store.Session{OwnerID: store.LocalUserID, ID: store.NewID(), Name: "chat", AgentConfigID: ac.ID}
	if err := sessions.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}

	info, err := runner.StartWorkflow(ctx, wf.ID, sess.ID, "the task", "")
	if err != nil {
		t.Fatalf("StartWorkflow: %v", err)
	}
	done, _ := awaitWorkflow(t, runner, info.TaskID, 20*time.Second)
	if done.Status != "completed" {
		t.Fatalf("status = %q (%s)", done.Status, done.Summary)
	}

	// The fold happens in the execution's own session, which is where the
	// steps' turns are.
	ref, err := store.RefFor(ctx, runner.db, done.ChildSessionID)
	if err != nil {
		t.Fatal(err)
	}
	views, err := store.NewEntryStoreFor(runner.db, ref).GetEntries(ctx, ref, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var folded int
	for _, v := range views {
		if v.Compacted {
			folded++
		}
	}
	if folded == 0 {
		t.Fatal("nothing was folded — the pass never ran before the step")
	}
}

// A failed execution retries from the step it stopped at, through the task
// retry: the failed step runs again (its run logged as one more launch), the
// remaining steps follow, and the retry clears a dismissal. A completed
// execution refuses to retry.
func TestRetryWorkflowResumesFromTheFailedStep(t *testing.T) {
	ctx := context.Background()
	var healthy atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !healthy.Load() {
			http.Error(w, `{"error":{"message":"model down"}}`, http.StatusInternalServerError)
			return
		}
		var body struct {
			Stream bool `json:"stream"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if !body.Stream {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(finishedResponse())
			return
		}
		send := sseWriter(w)
		sseCreated(send)
		send("response.completed", map[string]any{
			"type": "response.completed", "sequence_number": 1,
			"response": finishedResponse(),
		})
	}))
	defer srv.Close()
	runner, sess, wf := workflowFixture(t, srv.URL)

	info, err := runner.StartWorkflow(ctx, wf.ID, sess.ID, "", "")
	if err != nil {
		t.Fatalf("StartWorkflow: %v", err)
	}
	failed, st := awaitWorkflow(t, runner, info.TaskID, 20*time.Second)
	if failed.Status != "failed" || st.StepID != wf.Steps[0].ID {
		t.Fatalf("setup: %q at %q, want failed at the first step", failed.Status, st.StepID)
	}
	if won, err := runner.Deps.Tasks.Dismiss(ctx, info.TaskID); err != nil || !won {
		t.Fatalf("dismiss: won=%v err=%v", won, err)
	}

	healthy.Store(true)
	if _, err := runner.RetryTask(info.TaskID); err != nil {
		t.Fatalf("RetryTask: %v", err)
	}
	done, st := awaitWorkflow(t, runner, info.TaskID, 30*time.Second)
	if done.Status != "completed" {
		t.Fatalf("after retry: %q (%s), want completed", done.Status, done.Summary)
	}
	if done.Dismissed {
		t.Fatal("a retried execution is live again; the dismissal must clear")
	}
	// The log holds the failed launch AND the retry, plus the two later steps.
	if len(st.StepRuns) != 4 || st.StepRuns[0].StepID != wf.Steps[0].ID || st.StepRuns[1].StepID != wf.Steps[0].ID {
		t.Fatalf("step runs = %+v, want the first step twice then the rest", st.StepRuns)
	}
	if st.StepRuns[0].RunID == st.StepRuns[1].RunID {
		t.Fatal("the retry is a new run and must be logged as one")
	}
	// The failed launch's ending is stamped when its retry launches — the one
	// ending the sequence itself never moved on from.
	if st.StepRuns[0].Outcome != store.StepOutcomeFailed || st.StepRuns[1].Outcome != store.StepOutcomeCompleted {
		t.Fatalf("outcomes = %q,%q — want the failed launch stamped failed, its retry completed", st.StepRuns[0].Outcome, st.StepRuns[1].Outcome)
	}
	// The retry's turn re-issues the step's own instruction under the retry
	// prompt — a gate's rule and the step's task are not left to inference.
	ref, err := store.RefFor(ctx, runner.db, done.ChildSessionID)
	if err != nil {
		t.Fatal(err)
	}
	views, err := store.NewEntryStoreFor(runner.db, ref).GetEntries(ctx, ref, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var retryTurn string
	for _, v := range views {
		if v.RunID == st.StepRuns[1].RunID && v.Role == "user" {
			retryTurn = v.Content
			break
		}
	}
	if !strings.Contains(retryTurn, "A previous attempt at this task failed") || !strings.Contains(retryTurn, "The step to do again:\n") || !strings.Contains(retryTurn, wf.Steps[0].Prompt) {
		t.Fatalf("retry turn = %q, want the retry prompt followed by the step's own prompt", retryTurn)
	}

	// Completed executions never retry — re-running a success repeats its side
	// effects.
	if _, err := runner.RetryTask(info.TaskID); !errors.As(err, new(tasks.ErrNotRetryable)) {
		t.Fatalf("retry of a completed execution = %v, want ErrNotRetryable", err)
	}
}

// gateState is a two-step definition whose second step is a check that loops
// back to the first on FAIL, standing at the check.
func gateState(onFailure string) *store.WorkflowState {
	return &store.WorkflowState{
		Steps: store.WorkflowSteps{
			{ID: "work", AgentConfigID: "x", Prompt: "do it"},
			{ID: "check", AgentConfigID: "x", Prompt: "check it", Gate: &store.StepGate{}, OnFailure: onFailure, OnSuccess: store.WorkflowStepEnd},
		},
		StepID:   "check",
		StepRuns: store.StepRuns{{StepID: "work", RunID: "r1", Outcome: store.StepOutcomeCompleted}, {StepID: "check", RunID: "r2"}},
	}
}

// A gate step's verdict, not its run's outcome, chooses the edge — and the
// routing stays the definition's: PASS takes on_success, FAIL takes
// on_failure with the handler told why, and the launch log records which.
func TestGateVerdictRoutesTheEdges(t *testing.T) {
	// FAIL → the handler, led by the reason; the log says the check failed.
	st := gateState("work")
	cont, err := continueWorkflow(st, "r2", tasks.RunOutcome{Status: tasks.StatusCompleted, Text: "tests are red\nFAIL"}, 0)
	if err != nil || cont == nil {
		t.Fatalf("FAIL with a handler: cont=%v err=%v, want the on_failure edge", cont, err)
	}
	if !strings.HasPrefix(cont.Input, "The previous step failed: the check reported FAIL") || !strings.HasSuffix(cont.Input, "do it") {
		t.Fatalf("handler prompt = %q", cont.Input)
	}
	st, _ = store.DecodeWorkflowState(cont.State)
	if st.StepID != "work" || st.StepRuns[1].Outcome != store.StepOutcomeFail {
		t.Fatalf("after FAIL: at %q, log %+v", st.StepID, st.StepRuns)
	}

	// PASS → on_success, which here ends the execution with the run's outcome
	// — and the log still says how the last step ended.
	st = gateState("work")
	if cont, err := continueWorkflow(st, "r2", tasks.RunOutcome{Status: tasks.StatusCompleted, Text: "green\nPASS"}, 0); cont != nil || err != nil {
		t.Fatalf("PASS: cont=%v err=%v, want the execution to end cleanly", cont, err)
	}
	if st.StepRuns[1].Outcome != store.StepOutcomePass {
		t.Fatalf("after PASS: log %+v, want the last step recorded pass", st.StepRuns)
	}
	// No verdict at all is a broken check: the execution fails, saying so; the
	// run itself completed, and that is what the log keeps.
	st = gateState("work")
	if _, err := continueWorkflow(st, "r2", tasks.RunOutcome{Status: tasks.StatusCompleted, Text: "I think it is fine"}, 0); err == nil || !strings.Contains(err.Error(), "without a verdict") {
		t.Fatalf("no verdict: err = %v, want a refusal naming the missing verdict", err)
	}
	if st.StepRuns[1].Outcome != store.StepOutcomeCompleted {
		t.Fatalf("after no verdict: log %+v, want the run's own ending kept", st.StepRuns)
	}
	// A structural failure on a gate step is a failure like any other — no
	// verdict is read from a run that errored.
	st = gateState("work")
	if cont, err := continueWorkflow(st, "r2", tasks.RunOutcome{Status: tasks.StatusFailed, Err: "model down"}, 0); err != nil || cont == nil || !strings.Contains(cont.Input, "model down") {
		t.Fatalf("errored gate step: cont=%v err=%v, want the on_failure edge with the error", cont, err)
	}
	// FAIL with no handler cannot end with the run's (completed) outcome: the
	// execution fails, saying which check, and the log says fail.
	st = gateState("")
	if _, err := continueWorkflow(st, "r2", tasks.RunOutcome{Status: tasks.StatusCompleted, Text: "FAIL"}, 0); err == nil || !strings.Contains(err.Error(), "check") {
		t.Fatalf("FAIL without a handler: err = %v, want the execution failed by the check", err)
	}
	if st.StepRuns[1].Outcome != store.StepOutcomeFail {
		t.Fatalf("after unhandled FAIL: log %+v, want fail recorded", st.StepRuns)
	}
}

// scriptedModel answers call n with replies[n] (the last one repeats), as
// finished assistant messages — so a gate step can be made to say FAIL and
// then PASS.
func scriptedModel(t *testing.T, replies ...string) *httptest.Server {
	t.Helper()
	var calls atomic.Int32
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(calls.Add(1)) - 1
		if n >= len(replies) {
			n = len(replies) - 1
		}
		resp := finishedResponse()
		resp["output"].([]any)[0].(map[string]any)["content"] = []any{map[string]any{"type": "output_text", "text": replies[n], "annotations": []any{}}}
		var body struct {
			Stream bool `json:"stream"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if !body.Stream {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
			return
		}
		send := sseWriter(w)
		sseCreated(send)
		send("response.completed", map[string]any{"type": "response.completed", "sequence_number": 1, "response": resp})
	}))
}

// End to end: write → check(FAIL) → fix → check(PASS) → done, the fix loop a
// gate exists for, with the launch log carrying every verdict.
func TestWorkflowGateDrivesTheFixLoop(t *testing.T) {
	ctx := context.Background()
	srv := scriptedModel(t, "wrote it", "tests are red\nFAIL", "fixed it", "all green\nPASS")
	defer srv.Close()
	runner, sess, wf := workflowFixture(t, srv.URL)
	wf.Steps = store.WorkflowSteps{
		{ID: "write", Name: "write", AgentConfigID: wf.Steps[0].AgentConfigID, Prompt: "write it"},
		{ID: "check", Name: "check", AgentConfigID: wf.Steps[1].AgentConfigID, Prompt: "check it",
			Gate: &store.StepGate{}, OnSuccess: store.WorkflowStepEnd, OnFailure: "fix"},
		{ID: "fix", Name: "fix", AgentConfigID: wf.Steps[2].AgentConfigID, Prompt: "fix it", OnSuccess: "check"},
	}
	if err := runner.Deps.Workflows.Update(ctx, wf.ID, wf); err != nil {
		t.Fatal(err)
	}
	info, err := runner.StartWorkflow(ctx, wf.ID, sess.ID, "", "")
	if err != nil {
		t.Fatalf("StartWorkflow: %v", err)
	}
	done, st := awaitWorkflow(t, runner, info.TaskID, 20*time.Second)
	if done.Status != "completed" {
		t.Fatalf("status = %q (%s), want completed via the fix loop", done.Status, done.Summary)
	}
	var visited, outcomes []string
	for _, sr := range st.StepRuns {
		visited = append(visited, sr.StepID)
		outcomes = append(outcomes, sr.Outcome)
	}
	if strings.Join(visited, ",") != "write,check,fix,check" {
		t.Fatalf("visited %v, want write → check → fix → check", visited)
	}
	// Every run carries how it ended, the last one (the passing check) too.
	if strings.Join(outcomes, ",") != "completed,fail,completed,pass" {
		t.Fatalf("outcomes %v, want completed,fail,completed,pass", outcomes)
	}
}

// pausedFixture starts a workflow whose SECOND step must be approved, and
// returns once the execution is paused there.
func pausedFixture(t *testing.T, modelURL string) (*Runner, *store.Session, *tasks.Info, *store.PendingApproval) {
	t.Helper()
	ctx := context.Background()
	runner, sess, wf := workflowFixture(t, modelURL)
	wf.Steps[1].PauseBefore = true
	if err := runner.Deps.Workflows.Update(ctx, wf.ID, wf); err != nil {
		t.Fatal(err)
	}
	info, err := runner.StartWorkflow(ctx, wf.ID, sess.ID, "the brief", "")
	if err != nil {
		t.Fatalf("StartWorkflow: %v", err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		row, err := runner.Deps.Tasks.Get(ctx, info.TaskID)
		if err != nil {
			t.Fatal(err)
		}
		if row.Status == "input_required" {
			pending, err := runner.Deps.PendingApprovals.Get(ctx, row.RunID)
			if err != nil {
				t.Fatalf("paused task has no approval filed under its run: %v", err)
			}
			return runner, sess, info, pending
		}
		if isTerminalTaskStatus(row.Status) || time.Now().After(deadline) {
			t.Fatalf("execution never paused: %s (%s)", row.Status, row.Summary)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A pause names a run that never started, so its task.updated has no run
// stream to ride: it must reach connections through the broadcast hook, or the
// person is never asked until they reload.
func TestPausedStepIsAnnouncedWithoutARun(t *testing.T) {
	srv := oneShotModel(t)
	defer srv.Close()
	runner, sess, wf := workflowFixture(t, srv.URL)
	var mu sync.Mutex
	var seen []protocol.TaskUpdated
	runner.OnBroadcast = func(env *protocol.Envelope, _, _ string) {
		if env.Type != protocol.EventTaskUpdated {
			return
		}
		var upd protocol.TaskUpdated
		if json.Unmarshal(env.Payload, &upd) == nil {
			mu.Lock()
			seen = append(seen, upd)
			mu.Unlock()
		}
	}
	ctx := context.Background()
	wf.Steps[1].PauseBefore = true
	if err := runner.Deps.Workflows.Update(ctx, wf.ID, wf); err != nil {
		t.Fatal(err)
	}
	info, err := runner.StartWorkflow(ctx, wf.ID, sess.ID, "the brief", "")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		mu.Lock()
		var paused *protocol.TaskUpdated
		for i := range seen {
			if seen[i].TaskID == info.TaskID && seen[i].Status == "input_required" {
				paused = &seen[i]
			}
		}
		mu.Unlock()
		if paused != nil {
			if paused.PendingToolName != store.StepApprovalToolName || paused.PendingCallID == "" {
				t.Fatalf("broadcast pause = %+v, want the step approval named", paused)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the paused task was never broadcast")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A retry that lands on a pause_before step keeps the step's own instruction
// for the day it is approved: the pause stores the retry turn WITH the step
// prompt (composed before the branch), and the pause itself is one write —
// the state, the status and the approval together.
func TestRetryOfAPausedStepKeepsTheStepPrompt(t *testing.T) {
	ctx := context.Background()
	runner, sess, wf := workflowFixture(t, "http://127.0.0.1:1")
	wf.Steps[1].PauseBefore = true
	wf.Steps[1].Gate = &store.StepGate{}
	st := &store.WorkflowState{Steps: wf.Steps, StepID: wf.Steps[1].ID}
	row := &store.Task{
		ID: store.NewID(), RunID: store.NewID(), Kind: store.TaskKindWorkflow, State: st.Encode(),
		ParentSessionID: sess.ID, ChildSessionID: store.NewID(), Label: wf.Name, Status: "working",
	}
	if err := runner.Deps.Tasks.Create(ctx, row); err != nil {
		t.Fatal(err)
	}
	retryPrompt := "A previous attempt at this task failed: boom. Review it and continue."
	err := runner.launchWorkflowStep(ctx, tasks.LaunchRequest{
		TaskID: row.ID, Kind: row.Kind, State: row.State, RunID: row.RunID, SessionID: row.ChildSessionID,
		Input: retryPrompt, Retry: true,
	})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	after, _ := runner.Deps.Tasks.Get(ctx, row.ID)
	got, _ := store.DecodeWorkflowState(after.State)
	if after.Status != "input_required" {
		t.Fatalf("status = %s, want paused", after.Status)
	}
	if !strings.HasPrefix(got.PendingInput, retryPrompt) || !strings.Contains(got.PendingInput, "The step to do again:\n"+wf.Steps[1].Prompt) || !strings.Contains(got.PendingInput, store.DefaultGatePass) {
		t.Fatalf("pending turn = %q, want the retry prompt, the step's prompt and its gate rule", got.PendingInput)
	}
	if _, err := runner.Deps.PendingApprovals.Get(ctx, row.RunID); err != nil {
		t.Fatalf("the pause must file its approval in the same write: %v", err)
	}
	// A pause on a task no longer working on that run writes nothing at all —
	// neither a status nor an approval.
	other := &store.Task{
		ID: store.NewID(), RunID: store.NewID(), Kind: store.TaskKindWorkflow, State: st.Encode(),
		ParentSessionID: sess.ID, ChildSessionID: store.NewID(), Label: wf.Name, Status: "cancelled",
	}
	if err := runner.Deps.Tasks.Create(ctx, other); err != nil {
		t.Fatal(err)
	}
	if err := runner.launchWorkflowStep(ctx, tasks.LaunchRequest{TaskID: other.ID, Kind: other.Kind, State: other.State, RunID: other.RunID, SessionID: other.ChildSessionID, Input: "x"}); err == nil {
		t.Fatal("a pause on a task that moved on must be refused")
	}
	if _, err := runner.Deps.PendingApprovals.Get(ctx, other.RunID); err == nil {
		t.Fatal("no approval may be filed for a task the pause did not claim")
	}
}

// A decision on a step approval whose task has not (yet) been paused — the
// row exists, the task still says working under the same run — is refused as
// not ready and the row put back, so nothing is left paused with no approval
// to answer. (The launcher pauses first and files second, so this needs a
// store write to have failed or a client to have raced; either way the
// decision must not vanish.)
func TestStepApprovalOnAnUnpausedTaskIsNotReady(t *testing.T) {
	ctx := context.Background()
	runner, sess, wf := workflowFixture(t, "http://127.0.0.1:1")
	st := &store.WorkflowState{Steps: wf.Steps, StepID: wf.Steps[1].ID, PendingInput: "step two"}
	row := &store.Task{
		ID: store.NewID(), RunID: store.NewID(), Kind: store.TaskKindWorkflow, State: st.Encode(),
		ParentSessionID: sess.ID, ChildSessionID: store.NewID(), Label: wf.Name, Status: "working",
	}
	if err := runner.Deps.Tasks.Create(ctx, row); err != nil {
		t.Fatal(err)
	}
	pending := &store.PendingApproval{RunID: row.RunID, SessionID: row.ChildSessionID, Kind: store.ApprovalKindStep, AgentConfigID: wf.Steps[1].AgentConfigID}
	if err := runner.Deps.PendingApprovals.Save(ctx, pending); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.resolveStepApproval(ctx, pending, true); !errors.As(err, new(*ApprovalNotReadyError)) {
		t.Fatalf("approve of an unpaused step = %v, want ApprovalNotReadyError", err)
	}
	if _, err := runner.Deps.PendingApprovals.Get(ctx, row.RunID); err != nil {
		t.Fatalf("the approval must be put back for the decision to be retried: %v", err)
	}
	after, _ := runner.Deps.Tasks.Get(ctx, row.ID)
	if after.Status != "working" || after.RunID != row.RunID {
		t.Fatalf("task = %s/%s, want untouched", after.Status, after.RunID)
	}
}

// A step marked pause_before holds the sequence: the task is paused on a STEP
// approval (no run exists yet), the approval is answerable from the parent's
// list, and approving starts the step under the run id the pause filed — the
// rest of the sequence follows.
func TestWorkflowPausesBeforeAStepUntilApproved(t *testing.T) {
	ctx := context.Background()
	srv := oneShotModel(t)
	defer srv.Close()
	runner, sess, info, pending := pausedFixture(t, srv.URL)

	if pending.Kind != store.ApprovalKindStep || pending.State != "" {
		t.Fatalf("pause filed %+v, want a step approval with no run state", pending)
	}
	calls := pending.ParsedToolCalls()
	if len(calls) != 1 || calls[0].ToolName != store.StepApprovalToolName || !strings.Contains(calls[0].Arguments, "exec") {
		t.Fatalf("step approval calls = %+v, want one start_step naming the step", calls)
	}
	// Reachable from the conversation that asked, tagged with the execution.
	listed, err := runner.Deps.PendingApprovals.ListByParentTasks(ctx, sess.ID)
	if err != nil || len(listed) != 1 || listed[0].TaskID != info.TaskID {
		t.Fatalf("parent's approvals = %+v (%v), want the paused step's", listed, err)
	}
	// Nothing launched for the paused step: only the first step is in the log.
	if _, st := awaitState(t, runner, info.TaskID); len(st.StepRuns) != 1 || st.PendingInput == "" {
		t.Fatalf("paused state = %+v, want one launch and the pending turn kept", st)
	}

	// The restart sweep leaves a paused execution alone — the approval persists.
	runner.FailOrphanedTasks(ctx)
	if row, _ := runner.Deps.Tasks.Get(ctx, info.TaskID); row.Status != "input_required" {
		t.Fatalf("after the sweep: %s, want the pause kept", row.Status)
	}

	runID, _, err := runner.ResolveApproval(ctx, calls[0].ToolCallID, true, ApprovalOnce, "", nil)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if runID != pending.RunID {
		t.Fatalf("approve started %q, want the run the pause filed (%q)", runID, pending.RunID)
	}
	done, st := awaitWorkflow(t, runner, info.TaskID, 20*time.Second)
	if done.Status != "completed" {
		t.Fatalf("after approval: %q (%s), want completed", done.Status, done.Summary)
	}
	if len(st.StepRuns) != 3 || st.StepRuns[1].RunID != pending.RunID || st.PendingInput != "" {
		t.Fatalf("log after approval = %+v (pending %q), want three launches, the second under the pause's run", st.StepRuns, st.PendingInput)
	}
	// The approval was consumed; a second decision has nothing to act on.
	if _, _, err := runner.ResolveApproval(ctx, calls[0].ToolCallID, true, ApprovalOnce, "", nil); err == nil {
		t.Fatal("a consumed step approval must not be answerable twice")
	}
}

// Rejecting a step ends the execution: cancelled — the person's decision, so
// nobody is woken — and the approval is gone.
func TestWorkflowRejectedStepCancelsTheExecution(t *testing.T) {
	ctx := context.Background()
	srv := oneShotModel(t)
	defer srv.Close()
	runner, sess, info, pending := pausedFixture(t, srv.URL)
	calls := pending.ParsedToolCalls()

	runID, _, err := runner.ResolveApproval(ctx, calls[0].ToolCallID, false, ApprovalOnce, "not today", nil)
	if err != nil {
		t.Fatalf("reject: %v", err)
	}
	if runID != "" {
		t.Fatalf("reject started run %q, want none", runID)
	}
	row, err := runner.Deps.Tasks.Get(ctx, info.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != "cancelled" {
		t.Fatalf("after reject: %s, want cancelled", row.Status)
	}
	if _, err := runner.Deps.PendingApprovals.Get(ctx, pending.RunID); err == nil {
		t.Fatal("the rejected step's approval must be gone")
	}
	if pending, err := runner.Deps.Wakeups.Pending(ctx, sess.ID); err != nil || len(pending) != 0 {
		t.Fatalf("a rejection owes no wake-up, got %d (%v)", len(pending), err)
	}
}

// Stopping a paused execution discards the step approval with it, and a late
// decision on it is refused as void.
func TestWorkflowStopWhilePausedDropsTheStepApproval(t *testing.T) {
	ctx := context.Background()
	srv := oneShotModel(t)
	defer srv.Close()
	runner, _, info, pending := pausedFixture(t, srv.URL)

	if _, err := runner.StopTask(info.TaskID, false); err != nil {
		t.Fatalf("StopTask: %v", err)
	}
	if row, _ := runner.Deps.Tasks.Get(ctx, info.TaskID); row.Status != "cancelled" {
		t.Fatalf("after stop: %s, want cancelled", row.Status)
	}
	if _, err := runner.Deps.PendingApprovals.Get(ctx, pending.RunID); err == nil {
		t.Fatal("the stopped execution's step approval must be gone")
	}
	// A decision that had read the row before the stop is refused, not applied.
	if err := runner.Deps.PendingApprovals.Save(ctx, pending); err != nil {
		t.Fatal(err)
	}
	_, _, err := runner.ResolveApproval(ctx, pending.ParsedToolCalls()[0].ToolCallID, true, ApprovalOnce, "", nil)
	if _, void := errors.AsType[*ApprovalVoidError](err); !void {
		t.Fatalf("approve after stop = %v, want ApprovalVoidError", err)
	}
}

// awaitState reads a task's row and workflow state as they stand.
func awaitState(t *testing.T, runner *Runner, taskID string) (*store.Task, *store.WorkflowState) {
	t.Helper()
	row, err := runner.Deps.Tasks.Get(context.Background(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.DecodeWorkflowState(row.State)
	if err != nil {
		t.Fatal(err)
	}
	return row, st
}

// A person can run a workflow too — the same start the tool makes, with the
// brief they wrote — against a session that exists.
func TestRunWorkflowStartsAnExecutionForAPerson(t *testing.T) {
	ctx := context.Background()
	srv := oneShotModel(t)
	defer srv.Close()
	runner, sess, wf := workflowFixture(t, srv.URL)

	person := store.WorkflowOrigin{Kind: store.OriginPerson}
	if _, err := runner.RunWorkflow(ctx, wf.ID, "no-such-session", "brief", person); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown session: err = %v, want not found", err)
	}
	// A task's own (hidden) session is not a conversation to report to.
	hidden := &store.Session{OwnerID: store.LocalUserID, ID: store.NewID(), Name: "task", Hidden: true}
	if err := runner.Deps.Sessions.Create(ctx, hidden); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.RunWorkflow(ctx, wf.ID, hidden.ID, "brief", person); !errors.Is(err, ErrWorkflowUnavailable) {
		t.Fatalf("hidden session: err = %v, want the request refused", err)
	}
	info, err := runner.RunWorkflow(ctx, wf.ID, sess.ID, "sort a list", person)
	if err != nil {
		t.Fatalf("RunWorkflow: %v", err)
	}
	// With no run asking, the start leaves a note on the conversation — the
	// exchange's question, naming the task the result will come back for.
	ref, err := store.RefFor(ctx, runner.db, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	views, err := store.NewEntryStoreFor(runner.db, ref).GetEntries(ctx, ref, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var note *store.EntryView
	for i := range views {
		if views[i].Display != nil && views[i].Display.Kind == store.DisplayWorkflowStarted {
			note = &views[i]
		}
	}
	if note == nil {
		t.Fatalf("no workflow-started note among %d entries", len(views))
	}
	if note.Role != "system" || note.Display.Extra["task_id"] != info.TaskID || note.Display.Extra["brief"] != "sort a list" {
		t.Fatalf("note = role %q extra %v, want a system note naming the task and brief", note.Role, note.Display.Extra)
	}
	if info.Kind != store.TaskKindWorkflow || info.Label != wf.Name {
		t.Fatalf("started %+v, want a workflow task", info)
	}
	done, st := awaitWorkflow(t, runner, info.TaskID, 15*time.Second)
	if done.Status != "completed" || st.Input != "sort a list" {
		t.Fatalf("status = %q, input %q — want completed with the person's brief kept", done.Status, st.Input)
	}
	// The session never ran and is bound to no agent, so the result is
	// delivered by the workflow's own (first step's) agent rather than being
	// dropped as undeliverable.
	var inherits []string
	if err := runner.db.NewSelect().Model((*store.Wakeup)(nil)).Column("inherit").
		Where("session_id = ?", sess.ID).Scan(ctx, &inherits); err != nil {
		t.Fatal(err)
	}
	if len(inherits) != 1 || store.DecodeInherit([]byte(inherits[0])).AgentConfigID != wf.Steps[0].AgentConfigID {
		t.Fatalf("wake-up inherits = %v, want one carrying the first step's agent", inherits)
	}
}

// A backward edge is a loop, and a loop that keeps returning to the same step
// is not converging: the lap bound (default three, the definition's when set)
// ends the execution at the transition that would exceed it, saying which
// edge looped — long before the step ceiling, and only for backward edges;
// a forward edge taken over and over (a chain) is not a lap.
func TestWorkflowStopsAtTheLapBound(t *testing.T) {
	steps := store.WorkflowSteps{
		{ID: "plan", Name: "plan", AgentConfigID: "x", Prompt: "p"},
		{ID: "exec", Name: "exec", AgentConfigID: "x", Prompt: "e"},
		{ID: "verify", Name: "verify", AgentConfigID: "x", Prompt: "v", Gate: &store.StepGate{}, OnSuccess: store.WorkflowStepEnd, OnFailure: "exec"},
	}
	// The log after plan, then (exec, verify FAIL) four times: verify → exec
	// has been taken three times, and the verify run that just ended would
	// take it a fourth.
	st := &store.WorkflowState{Steps: steps, StepID: "verify"}
	st.StepRuns = st.StepRuns.With("plan", "r0")
	for i := range 4 {
		st.StepRuns = st.StepRuns.With("exec", fmt.Sprintf("e%d", i))
		st.StepRuns = st.StepRuns.With("verify", fmt.Sprintf("v%d", i))
	}
	last := st.StepRuns[len(st.StepRuns)-1].RunID
	fail := tasks.RunOutcome{Status: tasks.StatusCompleted, Text: "not there yet\nFAIL"}
	cont, err := continueWorkflow(st, last, fail, 0)
	if cont != nil || err == nil || !errors.Is(err, store.ErrLoopBound) || !strings.Contains(err.Error(), "verify → exec looped 3 times") {
		t.Fatalf("continue = %+v, %v — want the lap bound, naming the edge", cont, err)
	}
	if st.Stopped != store.StoppedByLaps {
		t.Fatalf("stopped = %q, want laps recorded on the state", st.Stopped)
	}
	// One lap fewer in the log, and the loop turns once more — the third lap.
	st = &store.WorkflowState{Steps: steps, StepID: "verify"}
	st.StepRuns = st.StepRuns.With("plan", "r0")
	for i := range 3 {
		st.StepRuns = st.StepRuns.With("exec", fmt.Sprintf("e%d", i))
		st.StepRuns = st.StepRuns.With("verify", fmt.Sprintf("v%d", i))
	}
	last = st.StepRuns[len(st.StepRuns)-1].RunID
	if cont, err := continueWorkflow(st, last, fail, 0); err != nil || cont == nil || st.StepID != "exec" {
		t.Fatalf("continue = %+v, %v — want one more lap to exec", cont, err)
	}
	// The definition raises the bound: the same log is under it.
	st = &store.WorkflowState{Steps: steps, StepID: "verify", Budget: store.WorkflowBudget{MaxLaps: 5}}
	st.StepRuns = st.StepRuns.With("plan", "r0")
	for i := range 4 {
		st.StepRuns = st.StepRuns.With("exec", fmt.Sprintf("e%d", i))
		st.StepRuns = st.StepRuns.With("verify", fmt.Sprintf("v%d", i))
	}
	last = st.StepRuns[len(st.StepRuns)-1].RunID
	if cont, err := continueWorkflow(st, last, fail, 0); err != nil || cont == nil {
		t.Fatalf("continue under a raised bound = %+v, %v — want a lap", cont, err)
	}
	// A forward chain is not a loop: a → b → c over and over never trips it
	// (the ceiling is what bounds a chain).
	chain := &store.WorkflowState{Steps: store.WorkflowSteps{
		{ID: "a", AgentConfigID: "x", Prompt: "a"}, {ID: "b", AgentConfigID: "x", Prompt: "b"},
	}, StepID: "a"}
	for i := range 10 {
		chain.StepRuns = chain.StepRuns.With("a", fmt.Sprintf("a%d", i))
		chain.StepRuns = chain.StepRuns.With("b", fmt.Sprintf("b%d", i))
	}
	chain.StepRuns = chain.StepRuns.With("a", "a-last")
	if cont, err := continueWorkflow(chain, "a-last", tasks.RunOutcome{Status: tasks.StatusCompleted}, 0); err != nil || cont == nil {
		t.Fatalf("a forward edge = %+v, %v — must not count as a lap", cont, err)
	}
}

// Retrying an execution the lap bound stopped is refused at the launch, like
// one the ceiling stopped: the state remembers the bound, and re-running the
// step could only arrive at the same transition.
func TestWorkflowRetryPastTheLapBoundIsRefused(t *testing.T) {
	ctx := context.Background()
	srv := oneShotModel(t)
	defer srv.Close()
	runner, sess, wf := workflowFixture(t, srv.URL)
	st := &store.WorkflowState{WorkflowID: wf.ID, Steps: wf.Steps, StepID: wf.Steps[0].ID, Stopped: store.StoppedByLaps}
	err := runner.launchWorkflowStep(ctx, tasks.LaunchRequest{
		TaskID: "t", RunID: store.NewID(), Kind: store.TaskKindWorkflow, State: st.Encode(), SessionID: sess.ID,
	})
	if !errors.Is(err, store.ErrLoopBound) {
		t.Fatalf("launch past the lap bound = %v — want the bound's refusal", err)
	}
}
