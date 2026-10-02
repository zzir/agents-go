package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/sandboxes"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/sandbox"
)

// execSeriesModel makes every turn run cmds through exec_command, one call per
// model request, and then answer: it counts the tool outputs after the
// request's last message to know where the turn stands.
func execSeriesModel(t *testing.T, cmds ...string) *httptest.Server {
	t.Helper()
	var callSeq atomic.Int32
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Input json.RawMessage `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		var items []struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(body.Input, &items)
		outputs := 0
	trailing:
		for i := len(items) - 1; i >= 0; i-- {
			switch items[i].Type {
			case "function_call_output":
				outputs++
			case "function_call", "reasoning":
			default:
				break trailing
			}
		}
		send := sseWriter(w)
		sseCreated(send)
		resp := finishedResponse()
		if outputs < len(cmds) {
			id := fmt.Sprintf("call_%d", callSeq.Add(1))
			args, _ := json.Marshal(map[string]any{"cmd": cmds[outputs], "timeout_seconds": 0, "workdir": "", "session_id": ""})
			resp["output"] = []any{map[string]any{
				"type": "function_call", "id": "fc_" + id, "call_id": id,
				"name": "exec_command", "arguments": string(args), "status": "completed",
			}}
		}
		send("response.completed", map[string]any{"type": "response.completed", "sequence_number": 1, "response": resp})
	}))
}

// awaitRunSettled waits for the run to leave "running" and returns where it stopped.
func awaitRunSettled(t *testing.T, runner *Runner, runID string) RunStatus {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		if info, ok := runner.hub.Info(runID); ok && info.Status != RunRunning {
			return info.Status
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s did not settle", runID)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// trustFixture wires a runner whose one agent gates exec_command, on a session
// bound to a sandbox that runs commands locally in a temp dir.
func trustFixture(t *testing.T, modelURL string) (*Runner, *store.Session, *store.AgentConfig) {
	t.Helper()
	ctx := context.Background()
	runner, sessions, _, agentConfigs := newTaskTestRunner(t)
	runner.Deps.Sandboxes = store.NewSandboxStore(runner.db)
	runner.Deps.Projects = store.NewProjectStore(runner.db)
	runner.Deps.SandboxManager = sandboxes.NewManager()
	workspace := t.TempDir()
	runner.Deps.SandboxManager.SetBuildOverride(func(sandboxes.Spec) (sandbox.Sandbox, error) {
		return sandbox.NewLocalWithOptions(sandbox.LocalOptions{WorkDir: workspace}), nil
	})
	tg := &store.Sandbox{ID: store.NewID(), Name: "host", Type: "docker", Config: []byte(`{"image":"i"}`)}
	if err := runner.Deps.Sandboxes.Create(ctx, tg); err != nil {
		t.Fatal(err)
	}
	proj := &store.Project{OwnerID: store.LocalUserID, SandboxID: tg.ID, Name: "p"}
	if err := runner.Deps.Projects.Create(ctx, proj); err != nil {
		t.Fatal(err)
	}
	ac := &store.AgentConfig{
		OwnerID: store.LocalUserID, Name: "operator", Model: "gpt-test",
		ProviderID: testProvider(t, runner.db, "endpoint", "k", modelURL),
		Approval:   store.ApprovalGroup{ApproveTools: store.StringList{"exec_command"}},
	}
	if err := agentConfigs.Create(ctx, ac); err != nil {
		t.Fatal(err)
	}
	sess := &store.Session{OwnerID: store.LocalUserID, ID: store.NewID(), Name: "chat", ProjectID: proj.ID}
	if err := sessions.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	return runner, sess, ac
}

// pausedCommand waits for the run to pause and returns the one call it waits on.
func pausedCommand(t *testing.T, runner *Runner, sessionID, runID string) store.PendingToolCall {
	t.Helper()
	if status := awaitRunSettled(t, runner, runID); status != RunInterrupted {
		t.Fatalf("run %s = %s, want it paused on a command", runID, status)
	}
	rows, err := runner.Deps.PendingApprovals.ListBySession(context.Background(), sessionID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("pending approvals = %d (%v), want one", len(rows), err)
	}
	var calls []store.PendingToolCall
	if err := json.Unmarshal(rows[0].ToolCalls, &calls); err != nil || len(calls) != 1 || calls[0].ToolName != "exec_command" {
		t.Fatalf("pending calls = %+v (%v), want one exec_command", calls, err)
	}
	return calls[0]
}

// decide answers the paused call with a scope and waits for the run to settle
// again: paused on the next command, or finished.
func decide(t *testing.T, runner *Runner, runID string, call store.PendingToolCall, scope ApprovalScope) *RunOutcome {
	t.Helper()
	done := make(chan *RunOutcome, 1)
	if _, _, err := runner.ResolveApproval(context.Background(), call.ToolCallID, true, scope, "", func(o *RunOutcome) { done <- o }); err != nil {
		t.Fatalf("resolve %s as %s: %v", call.ToolCallID, scope, err)
	}
	select {
	case out := <-done:
		return out
	case <-time.After(15 * time.Second):
		t.Fatalf("run %s did not settle after the decision", runID)
		return nil
	}
}

// A trigger's turn runs on its own grants (invariant 84). On a session whose
// person trusted everything it still asks for its first command, and each
// answer reaches exactly as far as its scope: once one call, same that
// command, all the rest of the run. The next fire starts from nothing.
func TestTriggerRunHonorsOnlyItsOwnCards(t *testing.T) {
	ctx := context.Background()
	srv := execSeriesModel(t, "echo a", "echo b", "echo b", "echo c", "echo d")
	defer srv.Close()
	runner, sess, ac := trustFixture(t, srv.URL)
	trust := runner.Deps.SandboxManager.Trust()
	trust.ForSession(sess.ID).AllowAll()

	// The person's own turn: every command runs on the session's grant.
	done := make(chan *RunOutcome, 1)
	personRun, err := runner.StartRun(sess.ID, ac.ID, "", TextInput("tidy up"), nil, func(o *RunOutcome) { done <- o })
	if err != nil {
		t.Fatal(err)
	}
	select {
	case out := <-done:
		if out.Interrupted || out.ErrCode != "" || out.FinalText != "done" {
			t.Fatalf("the person's turn = interrupted %v, %q %q, text %q; want it finished on the grant", out.Interrupted, out.ErrCode, out.ErrMessage, out.FinalText)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the person's turn did not finish")
	}
	if trust.RunTrust(personRun) != nil {
		t.Fatal("a person's turn was given a trust of its own")
	}

	sched := NewTriggerScheduler(runner, store.NewTriggerStore(runner.db))
	trg := &store.Trigger{Target: store.TriggerTargetAgent, AgentConfigID: ac.ID, SessionID: sess.ID, Kind: store.TriggerKindCron, Schedule: "@daily", Brief: "nightly tidy", Enabled: true}
	if err := sched.store.Create(ctx, trg); err != nil {
		t.Fatal(err)
	}
	fired, err := sched.Fire(ctx, trg.ID, "", FireCron)
	if err != nil {
		t.Fatalf("Fire: %v", err)
	}
	command := func(call store.PendingToolCall) string {
		var a struct {
			Cmd string `json:"cmd"`
		}
		_ = json.Unmarshal([]byte(call.Arguments), &a)
		return a.Cmd
	}

	// "echo a": asked despite the session's "all". Once reaches this call only.
	call := pausedCommand(t, runner, sess.ID, fired.RunID)
	if command(call) != "echo a" || trust.RunTrust(fired.RunID) == nil {
		t.Fatalf("first pause on %q (own trust %v), want echo a on a run with its own trust", command(call), trust.RunTrust(fired.RunID) != nil)
	}
	if out := decide(t, runner, fired.RunID, call, ApprovalOnce); !out.Interrupted {
		t.Fatalf("after approving once the turn = %+v, want it to ask for the next command", out)
	}
	// "echo b": asked. Same trusts this command, so its repeat runs unasked.
	call = pausedCommand(t, runner, sess.ID, fired.RunID)
	if command(call) != "echo b" {
		t.Fatalf("second pause on %q, want echo b", command(call))
	}
	if out := decide(t, runner, fired.RunID, call, ApprovalSameCommand); !out.Interrupted {
		t.Fatalf("after trusting one command the turn = %+v, want it to ask for a different one", out)
	}
	// "echo c": a different command, asked. All lets "echo d" and the rest run.
	call = pausedCommand(t, runner, sess.ID, fired.RunID)
	if command(call) != "echo c" {
		t.Fatalf("third pause on %q, want echo c — the repeated echo b should have run on the run's own grant", command(call))
	}
	if out := decide(t, runner, fired.RunID, call, ApprovalAll); out.Interrupted || out.ErrCode != "" || out.FinalText != "done" {
		t.Fatalf("after trusting all the turn = interrupted %v, %q %q, text %q; want it finished", out.Interrupted, out.ErrCode, out.ErrMessage, out.FinalText)
	}

	// The next fire is a new run: it asks again, from nothing.
	again, err := sched.Fire(ctx, trg.ID, "", FireCron)
	if err != nil {
		t.Fatalf("second Fire: %v", err)
	}
	if call = pausedCommand(t, runner, sess.ID, again.RunID); command(call) != "echo a" {
		t.Fatalf("the next fire paused on %q, want its first command", command(call))
	}
}

// A grant made on a trigger turn's card is the session's too: the person's
// own later turn runs on it.
func TestGrantOnATriggerCardAlsoServesThePerson(t *testing.T) {
	ctx := context.Background()
	srv := execSeriesModel(t, "echo a", "echo b")
	defer srv.Close()
	runner, sess, ac := trustFixture(t, srv.URL)

	sched := NewTriggerScheduler(runner, store.NewTriggerStore(runner.db))
	trg := &store.Trigger{Target: store.TriggerTargetAgent, AgentConfigID: ac.ID, SessionID: sess.ID, Kind: store.TriggerKindCron, Schedule: "@daily", Brief: "nightly tidy", Enabled: true}
	if err := sched.store.Create(ctx, trg); err != nil {
		t.Fatal(err)
	}
	fired, err := sched.Fire(ctx, trg.ID, "", FireCron)
	if err != nil {
		t.Fatalf("Fire: %v", err)
	}
	call := pausedCommand(t, runner, sess.ID, fired.RunID)
	if out := decide(t, runner, fired.RunID, call, ApprovalAll); out.Interrupted || out.FinalText != "done" {
		t.Fatalf("after trusting all the trigger's turn = %+v, want it finished", out)
	}

	done := make(chan *RunOutcome, 1)
	if _, err := runner.StartRun(sess.ID, ac.ID, "", TextInput("carry on"), nil, func(o *RunOutcome) { done <- o }); err != nil {
		t.Fatal(err)
	}
	select {
	case out := <-done:
		if out.Interrupted || out.ErrCode != "" || out.FinalText != "done" {
			t.Fatalf("the person's turn = interrupted %v, %q %q; want it to run on the grant made on the trigger's card", out.Interrupted, out.ErrCode, out.ErrMessage)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the person's turn did not finish")
	}
}

// Which segments run on their own grants (invariant 84).
func TestWithholdsTrust(t *testing.T) {
	ctx := context.Background()
	runner, sessions, tasks, _ := newTaskTestRunner(t)
	runner.Deps.SandboxManager = sandboxes.NewManager()
	parent := &store.Session{OwnerID: store.LocalUserID, ID: store.NewID(), Name: "chat"}
	if err := sessions.Create(ctx, parent); err != nil {
		t.Fatal(err)
	}
	withheldRun, personRun, newRun := store.NewID(), store.NewID(), store.NewID()
	runner.Deps.SandboxManager.Trust().WithholdRun(parent.ID, withheldRun)
	workflowTask := func(origin store.WorkflowOrigin) string {
		st := &store.WorkflowState{Steps: store.WorkflowSteps{{ID: "s1", Name: "one"}}, StepID: "s1", Origin: origin}
		row := &store.Task{
			ID: store.NewID(), RunID: store.NewID(), Kind: store.TaskKindWorkflow, State: st.Encode(),
			ParentSessionID: parent.ID, ChildSessionID: store.NewID(), Status: protocol.TaskWorking,
		}
		if err := tasks.Create(ctx, row); err != nil {
			t.Fatal(err)
		}
		return row.ID
	}
	fromTrigger := workflowTask(store.WorkflowOrigin{Kind: store.OriginTrigger, TriggerID: store.NewID()})
	fromPerson := workflowTask(store.WorkflowOrigin{Kind: store.OriginPerson})
	fromTool := workflowTask(store.WorkflowOrigin{})

	for _, tc := range []struct {
		name         string
		runID        string
		task         *TaskMeta
		fresh, asked bool
		want         bool
	}{
		{"a trigger's turn", newRun, nil, true, true, true},
		{"a trigger's turn resumed by a person's decision", withheldRun, nil, false, false, true},
		{"that resume after a restart forgot the run", newRun, nil, false, false, false},
		{"a person's turn", personRun, nil, true, false, false},
		{"a person's turn resumed", personRun, nil, false, false, false},
		{"a task a withheld run spawned", newRun, &TaskMeta{ParentRunID: withheldRun}, true, false, true},
		{"that task resumed", newRun, &TaskMeta{ParentRunID: withheldRun}, false, false, true},
		{"a task a person's run spawned", newRun, &TaskMeta{ParentRunID: personRun}, true, false, false},
		{"a trigger-started workflow's step", newRun, &TaskMeta{Kind: store.TaskKindWorkflow, TaskID: fromTrigger}, true, false, true},
		{"that step resumed", newRun, &TaskMeta{Kind: store.TaskKindWorkflow, TaskID: fromTrigger}, false, false, true},
		{"a person-started workflow's step", newRun, &TaskMeta{Kind: store.TaskKindWorkflow, TaskID: fromPerson}, true, false, false},
		{"a tool-started workflow's step in a person's run", newRun, &TaskMeta{Kind: store.TaskKindWorkflow, TaskID: fromTool, ParentRunID: personRun}, true, false, false},
		{"a tool-started workflow's step in a withheld run", newRun, &TaskMeta{Kind: store.TaskKindWorkflow, TaskID: fromTool, ParentRunID: withheldRun}, true, false, true},
		{"a workflow step whose row cannot be read", newRun, &TaskMeta{Kind: store.TaskKindWorkflow, TaskID: store.NewID()}, true, false, true},
	} {
		if got := runner.withholdsTrust(ctx, tc.runID, tc.task, tc.fresh, tc.asked); got != tc.want {
			t.Errorf("%s: withheld = %v, want %v", tc.name, got, tc.want)
		}
	}

	// A wake-up is withheld when ANY debt it delivers is of trigger-started
	// work, wherever in the batch it sits.
	subTask := &store.Task{ID: store.NewID(), RunID: store.NewID(), ParentSessionID: parent.ID, ChildSessionID: store.NewID(), ParentRunID: personRun, Status: protocol.TaskWorking}
	if err := tasks.Create(ctx, subTask); err != nil {
		t.Fatal(err)
	}
	ofPerson := store.Wakeup{SessionID: parent.ID, Kind: store.WakeKindTask, SourceID: subTask.ID, ParentRunID: personRun}
	ofWithheldRun := store.Wakeup{SessionID: parent.ID, Kind: store.WakeKindTask, SourceID: subTask.ID, ParentRunID: withheldRun}
	ofTriggerWorkflow := store.Wakeup{SessionID: parent.ID, Kind: store.WakeKindTask, SourceID: fromTrigger}
	ofPersonWorkflow := store.Wakeup{SessionID: parent.ID, Kind: store.WakeKindTask, SourceID: fromPerson}
	ofGoneTask := store.Wakeup{SessionID: parent.ID, Kind: store.WakeKindTask, SourceID: store.NewID(), ParentRunID: personRun}
	for _, tc := range []struct {
		name  string
		batch []store.Wakeup
		want  bool
	}{
		{"a person's task alone", []store.Wakeup{ofPerson}, false},
		{"a person-started workflow alone", []store.Wakeup{ofPersonWorkflow}, false},
		{"a withheld run's task behind a person's", []store.Wakeup{ofPerson, ofWithheldRun}, true},
		{"a trigger-started workflow behind a person's task", []store.Wakeup{ofPerson, ofTriggerWorkflow}, true},
		{"a trigger-started workflow alone", []store.Wakeup{ofTriggerWorkflow}, true},
		{"a debt whose task row is gone", []store.Wakeup{ofPerson, ofGoneTask}, true},
	} {
		if got := runner.wakeWithheld(ctx, tc.batch); got != tc.want {
			t.Errorf("wake-up for %s: withheld = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A trigger-started execution carries its origin on the state (a person's
// carries theirs), so each step's run and the wake-up reporting it are withheld.
func TestTriggerWorkflowStateCarriesOrigin(t *testing.T) {
	ctx := context.Background()
	srv := oneShotModel(t)
	defer srv.Close()
	runner, sess, wf, sched := triggerFixture(t, srv.URL)
	runner.Deps.SandboxManager = sandboxes.NewManager()
	trust := runner.Deps.SandboxManager.Trust()
	trg := &store.Trigger{WorkflowID: wf.ID, SessionID: sess.ID, Kind: store.TriggerKindCron, Schedule: "@daily", Brief: "nightly review", Enabled: true}
	if err := sched.store.Create(ctx, trg); err != nil {
		t.Fatal(err)
	}
	fired, err := sched.Fire(ctx, trg.ID, "", FireCron)
	if err != nil {
		t.Fatalf("Fire: %v", err)
	}
	row, st := awaitWorkflow(t, runner, fired.TaskID, 15*time.Second)
	if row.Status != "completed" {
		t.Fatalf("execution = %s (%s), want completed", row.Status, row.Summary)
	}
	if st.Origin.Kind != store.OriginTrigger || st.Origin.TriggerID != trg.ID {
		t.Fatalf("state origin = %+v, want this trigger", st.Origin)
	}
	if len(st.StepRuns) != len(wf.Steps) {
		t.Fatalf("step runs = %d, want %d", len(st.StepRuns), len(wf.Steps))
	}
	for _, sr := range st.StepRuns {
		if trust.RunTrust(sr.RunID) == nil {
			t.Errorf("step %s's run %s was not withheld", sr.StepID, sr.RunID)
		}
	}
	// The wake-up that reports the execution to the conversation is withheld too.
	ref := mustRef(t, runner.db, sess.ID)
	deadline := time.Now().Add(15 * time.Second)
	wakeRun := ""
	for wakeRun == "" {
		views, err := store.NewEntryStoreFor(runner.db, ref).GetEntries(ctx, ref)
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range views {
			if strings.HasPrefix(v.Content, protocol.TaskNotificationPrefix) {
				wakeRun = v.RunID
			}
		}
		if wakeRun == "" {
			if time.Now().After(deadline) {
				t.Fatal("no wake-up turn reported the execution")
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	if trust.RunTrust(wakeRun) == nil {
		t.Fatalf("the wake-up run %s reporting a trigger-started execution was not withheld", wakeRun)
	}
	awaitRunSettled(t, runner, wakeRun)

	// A person's own start carries the person, and is not withheld.
	info, err := runner.RunWorkflow(ctx, wf.ID, sess.ID, "by hand", store.WorkflowOrigin{Kind: store.OriginPerson})
	if err != nil {
		t.Fatalf("RunWorkflow: %v", err)
	}
	row, st = awaitWorkflow(t, runner, info.TaskID, 15*time.Second)
	if row.Status != "completed" || st.Origin.Kind != store.OriginPerson {
		t.Fatalf("a person's execution = %s, origin %+v; want completed, a person's", row.Status, st.Origin)
	}
	for _, sr := range st.StepRuns {
		if trust.RunTrust(sr.RunID) != nil {
			t.Errorf("a person-started step's run %s was withheld", sr.RunID)
		}
	}
}
