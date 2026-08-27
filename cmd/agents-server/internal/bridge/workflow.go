package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/zzir/agents-go/agents/tasks"
	"github.com/zzir/agents-go/cmd/agents-server/internal/logging"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// A workflow execution is a TASK of kind store.TaskKindWorkflow: the SDK's task
// manager owns its lifecycle — the child session, the run transitions, stop,
// retry, the restart sweep, the wake-up debt — and this file is the workflow
// DRIVER the manager calls back into: how an execution starts, which step a
// finished run leads to, and how a step's run is launched. workbench invariant 29.

// ErrWorkflowUnavailable marks a workflow a session cannot run right now — no
// steps, or an agent that no longer exists. The handler maps it to a 400.
var ErrWorkflowUnavailable = errors.New("workflow unavailable")

// StartWorkflow begins a workflow for a session: it snapshots the definition
// into the task's State and spawns a task of the workflow kind, whose first
// run is the first step. input is the brief — what this execution is about,
// written by the agent that asked for it; toolCallID is the spawn_task
// call, so the card it produced follows the execution (workbench invariant 30).
func (r *Runner) StartWorkflow(ctx context.Context, workflowID, parentSessionID, input, toolCallID string) (*tasks.Info, error) {
	if r.Deps.Workflows == nil || r.tasks == nil {
		return nil, errors.New("workflows are not wired")
	}
	wf, err := r.Deps.Workflows.Get(ctx, workflowID)
	if err != nil {
		return nil, err
	}
	if len(wf.Steps) == 0 {
		return nil, fmt.Errorf("%w: %q has no steps", ErrWorkflowUnavailable, wf.Name)
	}
	// Every step's agent is checked up front: finding out at step 4 that its
	// agent was deleted would leave the sequence half-run with no way to finish.
	for i := range wf.Steps {
		if _, err := r.Deps.AgentConfigs.Get(ctx, wf.Steps[i].AgentConfigID); err != nil {
			return nil, fmt.Errorf("%w: step %d (%s) names no agent", ErrWorkflowUnavailable, i+1, wf.Steps[i].Name)
		}
	}
	first := wf.Steps[0]
	state := &store.WorkflowState{
		WorkflowID: wf.ID,
		Steps:      wf.Steps, // the snapshot: editing the definition must not steer a run in flight
		Budget:     wf.Budget,
		Input:      input,
		StepID:     first.ID,
	}
	// The manager does the rest: the cap, the hidden child session, the row,
	// the launch (launchWorkflowStep, through the task launcher) and the
	// reconciliation with a stop that raced it. AgentName is the first step's
	// only so the resolver has something to name; each step brings its own.
	return r.tasks.Spawn(ctx, tasks.SpawnRequest{
		ParentSessionID: parentSessionID,
		AgentName:       first.AgentConfigID,
		Input:           state.StepPrompt(first),
		Label:           wf.Name,
		ParentRunID:     tasks.ParentRunID(ctx),
		ToolCallID:      toolCallID,
		Kind:            store.TaskKindWorkflow,
		State:           state.Encode(),
	})
}

// RunWorkflow starts a workflow for a session with no run asking — a person's
// own run of it (the REST endpoint), or a trigger's — with the brief written
// in advance. It is the same start the agent's tool makes, minus the call the
// tool's card would follow; in its place the start leaves a NOTE on the
// conversation (DisplayWorkflowStarted), which is what the result's wake-up
// run is then labeled by and jumps to.
func (r *Runner) RunWorkflow(ctx context.Context, workflowID, sessionID, input string, origin store.WorkflowOrigin) (*TaskInfo, error) {
	// The tool runs inside the session, so it exists; a request names one and
	// may be wrong — and a task row bound to no session would list nowhere.
	// A hidden session (a task's own) is refused too: it is already at the
	// task depth the manager allows, so the start could only fail there, as
	// a fault instead of a request error.
	sess, err := r.Deps.Sessions.Get(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if sess.Hidden {
		return nil, fmt.Errorf("%w: session %s is a task's own; a workflow reports to a conversation", ErrWorkflowUnavailable, sessionID)
	}
	info, err := r.StartWorkflow(ctx, workflowID, sessionID, input, "")
	if err != nil {
		return nil, err
	}
	// Best effort, after the start: the note names the task, and a start
	// that succeeded is not undone by a note that failed to write.
	if ref, rerr := store.RefFor(ctx, r.db, sessionID); rerr == nil {
		wf, _ := r.Deps.Workflows.Get(ctx, workflowID)
		name := workflowID
		if wf != nil {
			name = wf.Name
		}
		note := store.WorkflowStarted{TaskID: info.TaskID, WorkflowID: workflowID, WorkflowName: name, Brief: input, Origin: origin}
		if aerr := store.NewEntryStoreFor(r.db, ref).AppendWorkflowStarted(ctx, ref, note); aerr != nil {
			logging.Ctx(ctx).Warn("recording the workflow-started note", "error", aerr, "task_id", info.TaskID)
		}
		// A conversation that begins with a workflow has no first message for
		// the title generator to name it by; the workflow and its brief are
		// that name — otherwise it stays "New Session" everywhere a run is
		// listed by its conversation. Same CAS as the generator: a person's
		// name stands.
		if sess.Name == store.DefaultSessionName {
			r.nameSessionAfterWorkflow(ctx, sessionID, name, input)
		}
	}
	return r.taskInfoFrom(info), nil
}

// nameSessionAfterWorkflow names a still-default session "<workflow>: <brief>"
// and tells every connection, best effort.
func (r *Runner) nameSessionAfterWorkflow(ctx context.Context, sessionID, workflowName, brief string) {
	title := workflowName
	if b := strings.Join(strings.Fields(brief), " "); b != "" {
		title += ": " + b
	}
	if rs := []rune(title); len(rs) > 40 {
		title = string(rs[:39]) + "…"
	}
	won, err := r.Deps.Sessions.NameIfDefault(ctx, sessionID, title)
	if err != nil || !won || r.OnBroadcast == nil {
		return
	}
	if env, eerr := protocol.NewEnvelope(protocol.EventSessionTitleUpdated, protocol.SessionTitleUpdated{SessionID: sessionID, Title: title}); eerr == nil {
		r.OnBroadcast(env, "", sessionID)
	}
}

// continueTask is the task manager's Continue hook: only a workflow's run has
// a next step; every other task ends with its run. When the sequence ends
// here, the answer is an ENDING Continuation — no Input, the state carrying
// the last step's outcome (and the bound that stopped it), Err when it ends
// failed — which the manager writes in the same Finalize as the ending, so
// the record and the status cannot disagree.
func (r *Runner) continueTask(ctx context.Context, t *tasks.Task, out tasks.RunOutcome) (*tasks.Continuation, error) {
	if t.Kind != store.TaskKindWorkflow {
		return nil, nil
	}
	st, err := store.DecodeWorkflowState(t.State)
	if err != nil {
		return nil, err
	}
	tokens, err := r.executionTokens(ctx, st, t.ChildSessionID)
	if err != nil {
		return nil, err
	}
	cont, cerr := continueWorkflow(st, t.RunID, out, tokens)
	if cont != nil {
		return cont, nil
	}
	return &tasks.Continuation{State: st.Encode(), Err: cerr}, nil
}

// executionTokens is what the execution has spent so far, for its budget:
// the child session's usage totals. Not read for a workflow with no token
// bound — the count is only worth its query when something is measured by it.
func (r *Runner) executionTokens(ctx context.Context, st *store.WorkflowState, childSessionID string) (int, error) {
	if st.Budget.MaxTokens <= 0 || childSessionID == "" {
		return 0, nil
	}
	ref, err := store.RefFor(ctx, r.db, childSessionID)
	if err != nil {
		return 0, fmt.Errorf("reading the execution's usage: %w", err)
	}
	tokens, err := store.NewEntryStoreFor(r.db, ref).UsageTotals(ctx, ref)
	if err != nil {
		return 0, fmt.Errorf("reading the execution's usage: %w", err)
	}
	return tokens, nil
}

// continueWorkflow resolves where a workflow goes after the run that just
// ended: the next step by the definition's edges, or nowhere. It records the
// run's outcome on st either way. A structural failure with no handler ends
// the execution with the run's failure (nil, nil — the manager records the
// outcome); a gate's FAIL with no handler, a gate with no verdict, the budget
// and the loop bound end it with their own reason. tokens is what the
// execution has spent, for the budget.
func continueWorkflow(st *store.WorkflowState, runID string, out tasks.RunOutcome, tokens int) (*tasks.Continuation, error) {
	failed := out.Status == tasks.StatusFailed
	reason := out.Err
	outcome := store.StepOutcomeCompleted
	if failed {
		outcome = store.StepOutcomeFailed
	}
	// A gate step REPORTS; the routing is still the definition's. No verdict is
	// a broken check, not a coin flip.
	var noVerdict error
	if cur := st.Current(); cur != nil && cur.Gate != nil && !failed {
		passed, ok := cur.Gate.Verdict(out.Text)
		switch {
		case !ok:
			noVerdict = fmt.Errorf("step %q ended without a verdict: its last line must be %s or %s",
				stepName(cur), cur.Gate.PassWord(), cur.Gate.FailWord())
		case passed:
			outcome = store.StepOutcomePass
		default:
			failed, outcome = true, store.StepOutcomeFail
			reason = "the check reported " + cur.Gate.FailWord()
		}
	}
	st.RecordOutcome(runID, outcome)
	if noVerdict != nil {
		return nil, noVerdict
	}
	next, ok := st.NextStep(failed)
	if !ok {
		if outcome == store.StepOutcomeFail {
			// The run completed, so only this ending can say the check failed.
			return nil, fmt.Errorf("check %q failed", stepName(st.Current()))
		}
		return nil, nil
	}
	// The loop bound on the transition (a backward edge taken too often), then
	// the definition's budget, then the ceiling every execution has — counted
	// over step LAUNCHES.
	if err := st.StopIfLooping(next); err != nil {
		return nil, err
	}
	if err := st.StopIfBounded(tokens); err != nil {
		return nil, err
	}
	prompt := st.StepPrompt(*next)
	if failed {
		// A failed run leaves no usable account of itself in the transcript, so
		// the handler step is told what it is handling.
		prompt = "The previous step failed: " + reason + "\n\n" + prompt
	}
	st.StepID = next.ID
	return &tasks.Continuation{Input: prompt, State: st.Encode()}, nil
}

// stepName is how a step is named in a message: its name, else its id.
func stepName(s *store.WorkflowStep) string {
	if s == nil {
		return ""
	}
	if s.Name != "" {
		return s.Name
	}
	return s.ID
}

// launchWorkflowStep is the task launcher's answer for the workflow kind,
// whether the launch is the spawn, a step transition or a retry: start the
// step the task is on — or, for a step a person must approve first, hold the
// sequence there and ask (workbench invariant 37).
func (r *Runner) launchWorkflowStep(ctx context.Context, req tasks.LaunchRequest) error {
	st, err := store.DecodeWorkflowState(req.State)
	if err != nil {
		return err
	}
	step := st.Current()
	if step == nil {
		return fmt.Errorf("%w: step %q is not in the snapshot", ErrWorkflowUnavailable, st.StepID)
	}
	// A retry of an execution its budget or the ceiling stopped would run one
	// whole step only to be stopped by the same bound: refuse before the run,
	// and say so on the state (best effort — the refusal stands either way).
	tokens, err := r.executionTokens(ctx, st, req.SessionID)
	if err != nil {
		return err
	}
	if err := st.StopIfBounded(tokens); err != nil {
		_, _ = r.Deps.Tasks.Advance(ctx, req.TaskID, req.RunID, req.RunID, st.Encode())
		return err
	}
	// A retry's turn re-issues the step's own instruction under the retry
	// prompt: the transcript holds the failed attempt, but a gate's verdict
	// rule and the step's task are not something to leave the model to infer
	// from it. Composed HERE, before the branch, so a paused step keeps the
	// same turn for the day it is approved. A continuation's Input already is
	// the step prompt.
	if req.Retry {
		req.Input = req.Input + "\n\nThe step to do again:\n" + st.StepPrompt(*step)
	}
	if step.PauseBefore {
		return r.pauseWorkflowStep(ctx, req, st, step)
	}
	return r.startWorkflowStep(ctx, req, st, step)
}

// startWorkflowStep starts the step's run: it records the launch in the
// state's log first (atomically under the run it belongs to), compacts if the
// step asks, then starts the run as the step's agent on the parent's sandbox.
func (r *Runner) startWorkflowStep(ctx context.Context, req tasks.LaunchRequest, st *store.WorkflowState, step *store.WorkflowStep) error {
	// The log names every run that was launched, and only those: written
	// BEFORE the launch, under the run id the row already carries, so a stop
	// racing this launch loses the write rather than the run. A logged run the
	// sequence never moved on from is one that failed and is being retried —
	// the one ending Continue could not stamp, so it is stamped here.
	if n := len(st.StepRuns); n > 0 && st.StepRuns[n-1].Outcome == "" && st.StepRuns[n-1].RunID != req.RunID {
		st.StepRuns[n-1].Outcome = store.StepOutcomeFailed
	}
	if req.Retry {
		st.StepRuns = st.StepRuns.WithRetry(step.ID, req.RunID)
	} else {
		st.StepRuns = st.StepRuns.With(step.ID, req.RunID)
	}
	st.PendingInput = ""
	won, err := r.Deps.Tasks.Advance(ctx, req.TaskID, req.RunID, req.RunID, st.Encode())
	if err != nil {
		return fmt.Errorf("recording the step launch: %w", err)
	}
	if !won {
		return fmt.Errorf("%w: the execution moved on before the step could start", ErrWorkflowUnavailable)
	}
	if step.CompactBefore {
		// Best effort, before the launch while the child session is idle, with
		// the step's own agent — the one about to read the summary.
		rootCtx := r.hub.rootCtx
		ac, cerr := r.Deps.AgentConfigs.Get(rootCtx, step.AgentConfigID)
		if cerr == nil {
			_, _, _, cerr = r.compactSessionAs(rootCtx, req.SessionID, ac)
		}
		if cerr != nil {
			logging.Ctx(rootCtx).Warn("workflow: compact before step did not run", "error", cerr, "task_id", req.TaskID, "step_id", step.ID)
		}
	}
	// The sandbox comes from the PARENT (Inherit) — every step shares the
	// project of the conversation that asked; the agent is the step's own.
	in := store.DecodeInherit(req.Inherit)
	_, err = r.startRunWithID(req.RunID, req.SessionID, step.AgentConfigID, in.ProjectID, req.Input, "", nil, nil)
	return err
}

// pauseWorkflowStep holds the sequence before a PauseBefore step: the turn the
// step will start with is kept in the state, a step approval is filed under
// the run id the row already claimed, and the task is paused — all the
// machinery a tool approval has (the strip, the reaper, the restart sweep, a
// stop) then applies to it unchanged. Approving starts the run
// (resolveStepApproval); no run exists until then.
func (r *Runner) pauseWorkflowStep(ctx context.Context, req tasks.LaunchRequest, st *store.WorkflowState, step *store.WorkflowStep) error {
	if r.Deps.PendingApprovals == nil {
		return fmt.Errorf("%w: step %q asks for approval, but approvals are not persisted", ErrWorkflowUnavailable, stepName(step))
	}
	st.PendingInput = req.Input
	args, _ := json.Marshal(map[string]any{
		"step": stepName(step), "index": st.StepIndex(step.ID) + 1, "count": len(st.Steps),
	})
	calls, _ := json.Marshal([]store.PendingToolCall{{
		ToolCallID: "step-" + req.RunID, ToolName: store.StepApprovalToolName, Arguments: string(args),
	}})
	in := store.DecodeInherit(req.Inherit)
	// The state (the turn to start with), the pause and the approval land in
	// ONE transaction, under this run: no moment where the task is paused
	// with no approval to answer, or an approval answerable while the task
	// still says working. The manager reports the row as it now stands once
	// the launch settles, which is what tells every client to ask.
	won, err := r.Deps.Tasks.Pause(ctx, req.TaskID, req.RunID, st.Encode(), &store.PendingApproval{
		RunID: req.RunID, SessionID: req.SessionID, Kind: store.ApprovalKindStep,
		AgentConfigID: step.AgentConfigID, ProjectID: in.ProjectID,
		ToolCalls: calls,
	})
	if err != nil {
		return fmt.Errorf("pausing the task: %w", err)
	}
	if !won {
		return fmt.Errorf("%w: the execution moved on before the step could pause", ErrWorkflowUnavailable)
	}
	return nil
}

// resolveStepApproval applies a person's decision on a paused step. Approving
// reclaims the task and starts the step's run under the run id the pause
// filed; rejecting stops the execution — a declined step is the person ending
// the sequence, so it ends cancelled, owing no wake-up. The pending row's
// delete is the exclusive claim, as for a tool approval.
func (r *Runner) resolveStepApproval(ctx context.Context, pending *store.PendingApproval, approve bool) (runID string, err error) {
	mctx := context.WithoutCancel(ctx)
	row, err := r.Deps.Tasks.ByChildSession(ctx, pending.SessionID)
	if err != nil {
		return "", fmt.Errorf("resolving the paused step's execution: %w", err)
	}
	if !approve {
		// The claim and the ending in one write: the row deleted and the
		// execution cancelled — the person's decision, so nobody is woken.
		claimed, ended, err := r.Deps.Tasks.ClaimApprovalCancelled(mctx, row.ID, pending.RunID, "step rejected")
		if err != nil {
			return "", err
		}
		if !claimed {
			return "", fmt.Errorf("claiming the step approval: %w", store.ErrNotFound)
		}
		if ended {
			r.AnnounceTask(mctx, row.ID)
		}
		return "", nil
	}
	// The claim and the reclaim in one write: the row deleted (exclusive
	// against a racing decision) and the task input_required → working under
	// the pause's run id (exclusive against a concurrent stop). Nothing is
	// written when either side does not hold, so nothing needs putting back.
	outcome, err := r.Deps.Tasks.ClaimApprovalWorking(mctx, row.ID, pending.RunID)
	if err != nil {
		return "", fmt.Errorf("reclaiming the execution: %w", err)
	}
	switch outcome {
	case store.ClaimTaken:
		return "", fmt.Errorf("claiming the step approval: %w", store.ErrNotFound)
	case store.ClaimTaskNotPaused:
		// As for a tool approval: terminal → void; another attempt → stale;
		// still this attempt and not paused → not ready; the row is still
		// there for a later decision.
		if cur, gerr := r.Deps.Tasks.Get(mctx, row.ID); gerr == nil && !isTerminalTaskStatus(cur.Status) {
			if cur.RunID != pending.RunID {
				return "", &StaleApprovalAttemptError{TaskID: row.ID, ApprovalRunID: pending.RunID, CurrentRunID: cur.RunID}
			}
			return "", &ApprovalNotReadyError{RunID: pending.RunID}
		}
		return "", &ApprovalVoidError{TaskID: row.ID}
	}
	// From here the row is working again under the pause's run, so whatever
	// keeps the step from starting ends the execution failed, reported like
	// any ending — through the store, so the parent is owed the news — rather
	// than leaving a working row no run will ever finish. A finalize that
	// loses means a stop ended the execution first: the decision is void, a
	// state conflict rather than a fault.
	fail := func(reason string, err error) (string, error) {
		reason += ": " + err.Error()
		won, ferr := store.NewTaskAdapter(r.Deps.Tasks).Finalize(mctx, row.ID, pending.RunID, tasks.StatusFailed, reason, reason, nil)
		if ferr == nil && !won {
			return "", &ApprovalVoidError{TaskID: row.ID}
		}
		if ferr == nil {
			if t, gerr := store.NewTaskAdapter(r.Deps.Tasks).Get(mctx, row.ID); gerr == nil {
				r.onTaskUpdate(mctx, t)
			}
			(Waker{r}).Drain(mctx, row.ParentSessionID)
		}
		return "", err
	}
	st, err := store.DecodeWorkflowState(row.State)
	if err != nil {
		return fail("could not read the paused step", err)
	}
	step := st.Current()
	if step == nil {
		return fail("could not read the paused step", fmt.Errorf("%w: step %q is not in the snapshot", ErrWorkflowUnavailable, st.StepID))
	}
	req := tasks.LaunchRequest{
		TaskID: row.ID, Kind: row.Kind, State: row.State, RunID: pending.RunID, SessionID: row.ChildSessionID,
		Input: st.PendingInput,
		Inherit: store.EncodeInherit(store.Inherit{
			AgentConfigID: row.ParentAgentConfigID, ProjectID: row.ParentProjectID,
		}),
	}
	if lerr := r.startWorkflowStep(mctx, req, st, step); lerr != nil {
		return fail("could not start step "+stepName(step), lerr)
	}
	// Working again: tell the clients — the run's own run.started follows.
	if t, gerr := store.NewTaskAdapter(r.Deps.Tasks).Get(mctx, row.ID); gerr == nil {
		r.publishTaskUpdated(mctx, t)
	}
	return pending.RunID, nil
}
