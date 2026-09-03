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

// A workflow execution is a TASK of kind store.TaskKindWorkflow; the SDK's task
// manager owns its lifecycle and this file is the driver it calls — invariant 29.

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
	// The manager does the rest: cap, hidden child session, row, launch, stop
	// reconciliation. AgentName names the first step; each step brings its own.
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
	// A request may name a wrong session; a hidden (task's own) session is
	// refused too — a start there could only fail, as a fault.
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
		// A conversation begun by a workflow has no first message to be named
		// by, so the workflow and brief are its name. Same CAS as the generator.
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

// continueTask is the manager's Continue hook: only a workflow's run has a next
// step. An ENDING Continuation (no Input) carries the last outcome — invariant 31.
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

// executionTokens is what the execution has spent (the child session's usage
// totals); not read without a token bound.
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

// continueWorkflow resolves the next step after a run ended, recording its
// outcome on st; nil, nil ends the execution with the run's own failure.
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
	// The loop bound, the definition's budget, then the ceiling — counted
	// over step LAUNCHES (invariant 31).
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

// launchWorkflowStep is the task launcher's answer for the workflow kind (spawn,
// transition or retry): start the current step, or pause and ask — invariant 37.
func (r *Runner) launchWorkflowStep(ctx context.Context, req tasks.LaunchRequest) error {
	st, err := store.DecodeWorkflowState(req.State)
	if err != nil {
		return err
	}
	step := st.Current()
	if step == nil {
		return fmt.Errorf("%w: step %q is not in the snapshot", ErrWorkflowUnavailable, st.StepID)
	}
	// A retry past the budget or ceiling would run a whole step only to stop
	// again: refuse before the run, saying so on the state (best effort).
	tokens, err := r.executionTokens(ctx, st, req.SessionID)
	if err != nil {
		return err
	}
	if err := st.StopIfBounded(tokens); err != nil {
		_, _ = r.Deps.Tasks.Advance(ctx, req.TaskID, req.RunID, req.RunID, st.Encode())
		return err
	}
	// A retry re-issues the step's own instruction under the retry prompt,
	// composed HERE so a paused step keeps the same turn when approved.
	if req.Retry {
		req.Input = req.Input + "\n\nThe step to do again:\n" + st.StepPrompt(*step)
	}
	if step.PauseBefore {
		return r.pauseWorkflowStep(ctx, req, st, step)
	}
	return r.startWorkflowStep(ctx, req, st, step)
}

// startWorkflowStep records the launch in the state's log (under the run it
// belongs to), compacts if the step asks, then starts the run.
func (r *Runner) startWorkflowStep(ctx context.Context, req tasks.LaunchRequest, st *store.WorkflowState, step *store.WorkflowStep) error {
	// The log names every launched run, written BEFORE the launch under the
	// row's run id (invariant 31); a logged run never moved on from is a retried failure.
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
	_, err = r.startRunWithID(req.RunID, req.SessionID, step.AgentConfigID, in.ProjectID, TextInput(req.Input), "", nil, nil)
	return err
}

// pauseWorkflowStep holds the sequence before a PauseBefore step: the turn is
// kept in the state and a step approval filed — invariant 37. No run exists yet.
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
	// State, pause and approval land in ONE transaction under this run
	// (invariant 37); the manager reports the row once the launch settles.
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

// resolveStepApproval applies a decision on a paused step: approve reclaims the
// task and starts the run; reject cancels the execution — invariant 37.
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
	// Claim and reclaim in one write — invariant 37; nothing is written when
	// either side does not hold.
	outcome, err := r.Deps.Tasks.ClaimApprovalWorking(mctx, row.ID, pending.RunID)
	if err != nil {
		return "", fmt.Errorf("reclaiming the execution: %w", err)
	}
	switch outcome {
	case store.ClaimTaken:
		return "", fmt.Errorf("claiming the step approval: %w", store.ErrNotFound)
	case store.ClaimTaskNotPaused:
		// As for a tool approval: terminal → void; another attempt → stale;
		// this attempt not paused → not ready (the row stays).
		if cur, gerr := r.Deps.Tasks.Get(mctx, row.ID); gerr == nil && !isTerminalTaskStatus(cur.Status) {
			if cur.RunID != pending.RunID {
				return "", &StaleApprovalAttemptError{TaskID: row.ID, ApprovalRunID: pending.RunID, CurrentRunID: cur.RunID}
			}
			return "", &ApprovalNotReadyError{RunID: pending.RunID}
		}
		return "", &ApprovalVoidError{TaskID: row.ID}
	}
	// Working again, so a failure to start ends the execution failed via the
	// store (the parent is owed the news); a lost finalize means a stop won: void.
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
