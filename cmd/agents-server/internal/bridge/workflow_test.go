package bridge

import (
	"context"
	"encoding/json"
	"errors"
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
	runner.Deps.WorkflowRuns = store.NewWorkflowRunStore(runner.db)
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
	sess := &store.Session{ID: store.NewID(), Name: "chat"}
	if err := sessions.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	return runner, sess, wf
}

// awaitWorkflow waits for the execution to leave "running".
func awaitWorkflow(t *testing.T, runner *Runner, id string, within time.Duration) *store.WorkflowRun {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		wr, err := runner.Deps.WorkflowRuns.Get(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if wr.Status != store.WorkflowRunning || time.Now().After(deadline) {
			return wr
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

	wr, err := runner.StartWorkflow(ctx, wf.ID, sess.ID, "sort a list")
	if err != nil {
		t.Fatalf("StartWorkflow: %v", err)
	}
	if wr.StepID != wf.Steps[0].ID {
		t.Fatalf("started at %q, want the first step", wr.StepID)
	}

	done := awaitWorkflow(t, runner, wr.ID, 15*time.Second)
	if done.Status != store.WorkflowCompleted {
		t.Fatalf("status = %q (%s), want completed", done.Status, done.Error)
	}
	if done.StepID != wf.Steps[2].ID {
		t.Fatalf("ended at step %q, want the last one", done.StepID)
	}
	// The run log keeps every step, not just the current one — it is what maps
	// a finished sequence's turns back to the step that produced them.
	if len(done.StepRuns) != 3 {
		t.Fatalf("step runs = %+v, want one per step", done.StepRuns)
	}
	for i, sr := range done.StepRuns {
		if sr.StepID != wf.Steps[i].ID || sr.RunID == "" {
			t.Fatalf("step run %d = %+v, want step %q with a run", i, sr, wf.Steps[i].ID)
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
// row keeps the failed step so a retry resumes from it.
func TestWorkflowStopsAtAFailedStep(t *testing.T) {
	ctx := context.Background()
	// No model server: the first step's call fails.
	runner, sess, wf := workflowFixture(t, "http://127.0.0.1:1")

	wr, err := runner.StartWorkflow(ctx, wf.ID, sess.ID, "")
	if err != nil {
		t.Fatalf("StartWorkflow: %v", err)
	}
	done := awaitWorkflow(t, runner, wr.ID, 20*time.Second)
	if done.Status != store.WorkflowFailed {
		t.Fatalf("status = %q, want failed", done.Status)
	}
	if done.StepID != wf.Steps[0].ID {
		t.Fatalf("failed at step %q, want the first one — a retry resumes from here", done.StepID)
	}
	if done.Error == "" {
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

	wr, err := runner.StartWorkflow(tasks.WithParentRunID(ctx, "run_asked"), wf.ID, sess.ID, "")
	if err != nil {
		t.Fatalf("StartWorkflow: %v", err)
	}
	if wr.OriginRunID != "run_asked" {
		t.Fatalf("origin run = %q, want the run whose tool call started it", wr.OriginRunID)
	}
	// The inherit is frozen at start (the parent's agent), so the result comes
	// back through the agent that asked even if the session is re-pointed later.
	if wr.Inherit == "" {
		t.Fatal("the execution must snapshot the configuration to deliver its result under")
	}
}

// A finished workflow's wake-up lands in the SAME transaction as its terminal
// state, so a crash can never leave a completed sequence whose parent is never
// told — and a cancellation owes nothing.
func TestWorkflowFinishWritesWakeupAtomically(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	wfr := store.NewWorkflowRunStore(db)
	wakeups := store.NewWakeupStore(db)

	mk := func() *store.WorkflowRun {
		wr := &store.WorkflowRun{
			ID: store.NewID(), ParentSessionID: store.NewID(), ChildSessionID: store.NewID(),
			Name: "codegen", RunID: store.NewID(), Status: store.WorkflowRunning,
			Inherit: string(store.EncodeInherit(store.Inherit{AgentConfigID: "ac"})),
		}
		if err := wfr.Create(ctx, wr); err != nil {
			t.Fatal(err)
		}
		return wr
	}

	done := mk()
	wk := &store.Wakeup{SessionID: done.ParentSessionID, Kind: store.WakeKindWorkflow, SourceID: done.ID,
		Inherit: done.Inherit, Payload: "Workflow \"codegen\" completed."}
	if won, err := wfr.Finish(ctx, done.ID, done.RunID, store.WorkflowCompleted, "", "out", wk); err != nil || !won {
		t.Fatalf("Finish won=%v err=%v", won, err)
	}
	if p, err := wakeups.Pending(ctx, done.ParentSessionID); err != nil || len(p) != 1 {
		t.Fatalf("completed workflow owed %d wake-ups (err=%v), want 1", len(p), err)
	}

	// A lost CAS (wrong fromRunID) writes neither the status nor the wake-up.
	stale := mk()
	if won, err := wfr.Finish(ctx, stale.ID, "not-its-run", store.WorkflowCompleted, "", "x",
		&store.Wakeup{SessionID: stale.ParentSessionID, Kind: store.WakeKindWorkflow, SourceID: stale.ID, Payload: "x"}); err != nil || won {
		t.Fatalf("Finish(stale) won=%v err=%v, want won=false", won, err)
	}
	if p, err := wakeups.Pending(ctx, stale.ParentSessionID); err != nil || len(p) != 0 {
		t.Fatalf("a lost CAS owed %d wake-ups (err=%v), want 0", len(p), err)
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

	wr, err := runner.StartWorkflow(ctx, wf.ID, sess.ID, "")
	if err != nil {
		t.Fatalf("StartWorkflow: %v", err)
	}
	done := awaitWorkflow(t, runner, wr.ID, 20*time.Second)
	if done.Status != store.WorkflowCompleted {
		t.Fatalf("status = %q (%s), want completed — the failure edge should have carried it", done.Status, done.Error)
	}
	// work, recover, work: the log is what proves a step ran twice.
	var visited []string
	for _, sr := range done.StepRuns {
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
// that never succeeds. The ceiling counts step RUNS, because that is what costs.
func TestWorkflowStopsAtTheStepCeiling(t *testing.T) {
	ctx := context.Background()
	srv := oneShotModel(t)
	defer srv.Close()
	runner, sess, wf := workflowFixture(t, srv.URL)

	wr := &store.WorkflowRun{
		ParentSessionID: sess.ID, ChildSessionID: store.NewID(), Name: wf.Name, Steps: wf.Steps,
		StepID: wf.Steps[0].ID, RunID: "run_now", Status: store.WorkflowRunning,
	}
	for range store.MaxStepRuns {
		wr.StepRuns = wr.StepRuns.With(wf.Steps[0].ID, store.NewID())
	}
	if err := runner.Deps.WorkflowRuns.Create(ctx, wr); err != nil {
		t.Fatal(err)
	}
	runner.advanceWorkflow(ctx, "run_now", &RunOutcome{FinalText: "another lap"})

	after, err := runner.Deps.WorkflowRuns.Get(ctx, wr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != store.WorkflowFailed || !strings.Contains(after.Error, "looping") {
		t.Fatalf("status = %q, error = %q — want it stopped for looping", after.Status, after.Error)
	}
}

// Stopping a workflow paused on an approval deletes that approval, so a stale
// tool_call_id cannot resume the step into a cancelled sequence — and the
// resume guard refuses even if a resume read the row first.
func TestStopWorkflowClearsAPausedApproval(t *testing.T) {
	ctx := context.Background()
	srv := oneShotModel(t)
	defer srv.Close()
	runner, sess, wf := workflowFixture(t, srv.URL)
	approvals := store.NewPendingApprovalStore(runner.db)
	runner.Deps.PendingApprovals = approvals

	child := store.NewID()
	runID := store.NewID()
	wr := &store.WorkflowRun{
		ParentSessionID: sess.ID, ChildSessionID: child, Name: wf.Name, Steps: wf.Steps,
		StepID: wf.Steps[0].ID, RunID: runID, Status: store.WorkflowRunning,
	}
	if err := runner.Deps.WorkflowRuns.Create(ctx, wr); err != nil {
		t.Fatal(err)
	}
	// The step is paused: a pending approval keyed by the child run.
	calls, _ := json.Marshal([]store.PendingToolCall{{ToolCallID: "call_x", ToolName: "write_file", Arguments: "{}"}})
	if err := approvals.Save(ctx, &store.PendingApproval{
		RunID: runID, SessionID: child, State: "{}", ToolCalls: calls,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := runner.StopWorkflow(ctx, wr.ID); err != nil {
		t.Fatalf("StopWorkflow: %v", err)
	}
	if got, _, err := approvals.FindByToolCall(ctx, "call_x"); err == nil && got != nil {
		t.Fatal("the stopped step's approval must be gone")
	}

	// Even a resume that had already read the row is refused now that the
	// workflow is terminal — re-plant it and try.
	if err := approvals.Save(ctx, &store.PendingApproval{
		RunID: runID, SessionID: child, State: "{}", ToolCalls: calls,
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runner.ResolveApproval(ctx, "call_x", true, ApprovalOnce, "", nil); !errors.Is(err, ErrWorkflowUnavailable) {
		t.Fatalf("resume of a stopped workflow's step = %v, want ErrWorkflowUnavailable", err)
	}
	if got, _, err := approvals.FindByToolCall(ctx, "call_x"); err == nil && got != nil {
		t.Fatal("the refused approval must have been discarded")
	}
}

// The outcome comes back as a turn the SERVER injected, so it has to carry the
// notification prefix: without it the UI renders the sequence's result as a
// message the person typed.
func TestWorkflowWakePayloadIsMarkedAsANotification(t *testing.T) {
	got := workflowWakePayload(&store.WorkflowRun{Name: "codegen"}, store.WorkflowCompleted, "", "wrote fib.py")
	if !strings.HasPrefix(got, protocol.TaskNotificationPrefix) {
		t.Fatalf("payload = %q, want the notification prefix", got)
	}
	if !strings.Contains(got, `"codegen"`) || !strings.Contains(got, "wrote fib.py") {
		t.Fatalf("payload = %q, want it to name the workflow and carry the result", got)
	}
}

// The advance is a compare-and-set on the run the row believes is current: a
// late callback from a superseded run must not move the sequence on.
func TestWorkflowAdvanceIgnoresASupersededRun(t *testing.T) {
	ctx := context.Background()
	srv := oneShotModel(t)
	defer srv.Close()
	runner, sess, wf := workflowFixture(t, srv.URL)

	wr := &store.WorkflowRun{
		ParentSessionID: sess.ID, ChildSessionID: store.NewID(), Name: wf.Name, Steps: wf.Steps,
		StepID: wf.Steps[0].ID, RunID: "run_current", Status: store.WorkflowRunning,
	}
	if err := runner.Deps.WorkflowRuns.Create(ctx, wr); err != nil {
		t.Fatal(err)
	}
	ok, err := runner.Deps.WorkflowRuns.Advance(ctx, wr.ID, "run_stale", wf.Steps[1].ID, "run_next", nil)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("an advance from a superseded run must not apply")
	}
	after, err := runner.Deps.WorkflowRuns.Get(ctx, wr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.StepID != wf.Steps[0].ID || after.RunID != "run_current" {
		t.Fatalf("the row moved under a stale advance: %+v", after)
	}
}

// Background work is budgeted, not forbidden: the sequences run on sessions of
// their own, so what bounds them is the same per-session cap tasks answer to.
func TestWorkflowRefusesPastTheBackgroundBudget(t *testing.T) {
	ctx := context.Background()
	srv := oneShotModel(t)
	defer srv.Close()
	runner, sess, wf := workflowFixture(t, srv.URL)

	for range runner.hub.maxTasks {
		if err := runner.Deps.WorkflowRuns.Create(ctx, &store.WorkflowRun{
			ParentSessionID: sess.ID, ChildSessionID: store.NewID(), Name: "other", Steps: wf.Steps,
			StepID: wf.Steps[0].ID, RunID: store.NewID(), Status: store.WorkflowRunning,
		}); err != nil {
			t.Fatal(err)
		}
	}
	_, err := runner.StartWorkflow(ctx, wf.ID, sess.ID, "")
	if err == nil {
		t.Fatal("a workflow past the background budget must be refused")
	}
	if !errors.Is(err, ErrWorkflowUnavailable) {
		t.Fatalf("err = %v, want ErrWorkflowUnavailable", err)
	}
}

// The definition may be edited or deleted while an execution is in flight: the
// run carries its own snapshot and keeps running what it started with.
func TestWorkflowRunCarriesItsOwnSnapshot(t *testing.T) {
	ctx := context.Background()
	srv := oneShotModel(t)
	defer srv.Close()
	runner, sess, wf := workflowFixture(t, srv.URL)

	wr, err := runner.StartWorkflow(ctx, wf.ID, sess.ID, "")
	if err != nil {
		t.Fatalf("StartWorkflow: %v", err)
	}
	if err := runner.Deps.Workflows.Delete(ctx, wf.ID); err != nil {
		t.Fatal(err)
	}
	done := awaitWorkflow(t, runner, wr.ID, 15*time.Second)
	if done.Status != store.WorkflowCompleted {
		t.Fatalf("status = %q (%s), want completed despite the deleted definition", done.Status, done.Error)
	}
	if len(done.Steps) != 3 {
		t.Fatalf("the snapshot should still hold every step, got %d", len(done.Steps))
	}
}

// A restart leaves no live step, so an execution recorded as running is failed
// at the step it reached — which a retry resumes from.
func TestFailInterruptedWorkflowsAfterRestart(t *testing.T) {
	ctx := context.Background()
	runner, sess, wf := workflowFixture(t, "http://127.0.0.1:1")

	wr := &store.WorkflowRun{
		ParentSessionID: sess.ID, ChildSessionID: store.NewID(), Name: wf.Name, Steps: wf.Steps,
		StepID: wf.Steps[1].ID, RunID: "run_gone", Status: store.WorkflowRunning,
	}
	if err := runner.Deps.WorkflowRuns.Create(ctx, wr); err != nil {
		t.Fatal(err)
	}
	runner.FailInterruptedWorkflows(ctx)

	after, err := runner.Deps.WorkflowRuns.Get(ctx, wr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != store.WorkflowFailed {
		t.Fatalf("status = %q, want failed", after.Status)
	}
	if after.StepID != wf.Steps[1].ID {
		t.Fatalf("the reached step must survive for a retry, got %q", after.StepID)
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
// from the driver, i.e. inside the previous run's TEARDOWN — after the hub
// released the session, or the busy check would refuse it and the flag would
// quietly do nothing.
func TestWorkflowCompactsBeforeAStep(t *testing.T) {
	ctx := context.Background()
	srv := oneShotModel(t)
	defer srv.Close()
	runner, sessions, _, agentConfigs := newTaskTestRunner(t)
	runner.Deps.Workflows = store.NewWorkflowStore(runner.db)
	runner.Deps.WorkflowRuns = store.NewWorkflowRunStore(runner.db)
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
	sess := &store.Session{ID: store.NewID(), Name: "chat", AgentConfigID: ac.ID}
	if err := sessions.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}

	wr, err := runner.StartWorkflow(ctx, wf.ID, sess.ID, "the task")
	if err != nil {
		t.Fatalf("StartWorkflow: %v", err)
	}
	done := awaitWorkflow(t, runner, wr.ID, 20*time.Second)
	if done.Status != store.WorkflowCompleted {
		t.Fatalf("status = %q (%s)", done.Status, done.Error)
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

// A failed execution retries from the step it stopped at: the failed step runs
// again (its run logged as one more attempt), the remaining steps follow, and
// the retry clears a dismissal. A completed execution refuses to retry.
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

	wr, err := runner.StartWorkflow(ctx, wf.ID, sess.ID, "")
	if err != nil {
		t.Fatalf("StartWorkflow: %v", err)
	}
	failed := awaitWorkflow(t, runner, wr.ID, 20*time.Second)
	if failed.Status != store.WorkflowFailed || failed.StepID != wf.Steps[0].ID {
		t.Fatalf("setup: %q at %q, want failed at the first step", failed.Status, failed.StepID)
	}
	if won, err := runner.Deps.WorkflowRuns.Dismiss(ctx, wr.ID); err != nil || !won {
		t.Fatalf("dismiss: won=%v err=%v", won, err)
	}

	healthy.Store(true)
	if _, err := runner.RetryWorkflow(ctx, wr.ID); err != nil {
		t.Fatalf("RetryWorkflow: %v", err)
	}
	done := awaitWorkflow(t, runner, wr.ID, 30*time.Second)
	if done.Status != store.WorkflowCompleted {
		t.Fatalf("after retry: %q (%s), want completed", done.Status, done.Error)
	}
	if done.Dismissed {
		t.Fatal("a retried execution is live again; the dismissal must clear")
	}
	// The log holds the failed attempt AND the retry, plus the two later steps.
	if len(done.StepRuns) != 4 || done.StepRuns[0].StepID != wf.Steps[0].ID || done.StepRuns[1].StepID != wf.Steps[0].ID {
		t.Fatalf("step runs = %+v, want the first step twice then the rest", done.StepRuns)
	}

	// Completed executions never retry — re-running a success repeats its side
	// effects.
	if _, err := runner.RetryWorkflow(ctx, wr.ID); !errors.Is(err, ErrWorkflowUnavailable) {
		t.Fatalf("retry of a completed execution = %v, want ErrWorkflowUnavailable", err)
	}
}

// An expired approval ends the execution whose step was waiting on it — failed
// at that step, the parent owed a wake-up — and a second sweep is a no-op.
func TestFailWorkflowForExpiredApproval(t *testing.T) {
	ctx := context.Background()
	runner, sessions, _, _ := newTaskTestRunner(t)
	runner.Deps.WorkflowRuns = store.NewWorkflowRunStore(runner.db)

	parent := &store.Session{ID: store.NewID(), Name: "chat"}
	child := &store.Session{ID: store.NewID(), Name: "wf", Hidden: true}
	for _, s := range []*store.Session{parent, child} {
		if err := sessions.Create(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	wr := &store.WorkflowRun{
		ParentSessionID: parent.ID, ChildSessionID: child.ID, Name: "wf",
		Steps:  store.WorkflowSteps{{ID: "s1", AgentConfigID: "ac", Prompt: "p"}},
		StepID: "s1", RunID: "r1",
		Inherit: string(store.EncodeInherit(store.Inherit{AgentConfigID: "ac"})),
		Status:  store.WorkflowRunning,
	}
	if err := runner.Deps.WorkflowRuns.Create(ctx, wr); err != nil {
		t.Fatal(err)
	}

	runner.FailWorkflowForExpiredApproval(ctx, child.ID)
	got, err := runner.Deps.WorkflowRuns.Get(ctx, wr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.WorkflowFailed || !strings.Contains(got.Error, "approval expired") {
		t.Fatalf("after expiry: %q (%q), want failed with the reason", got.Status, got.Error)
	}
	// The debt was recorded in the same tx (and may already be delivered — the
	// idle parent is drained immediately), so count rows, not pending state.
	countDebts := func() int {
		n, cerr := runner.db.NewSelect().Model((*store.Wakeup)(nil)).
			Where("session_id = ?", parent.ID).
			Where("kind = ?", store.WakeKindWorkflow).Count(ctx)
		if cerr != nil {
			t.Fatal(cerr)
		}
		return n
	}
	if n := countDebts(); n != 1 {
		t.Fatalf("workflow debts = %d, want exactly one", n)
	}

	// Terminal already: the sweep finds nothing running and changes nothing.
	runner.FailWorkflowForExpiredApproval(ctx, child.ID)
	if n := countDebts(); n != 1 {
		t.Fatalf("a second sweep must not owe a second debt, got %d", n)
	}
}
