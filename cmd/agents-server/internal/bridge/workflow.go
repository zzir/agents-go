package bridge

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/rs/zerolog"

	"github.com/zzir/agents-go/agents/tasks"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// ErrWorkflowUnavailable marks a workflow a session cannot run right now — no
// steps, an agent that no longer exists, or a background budget already spent.
// The handler maps it to a 400.
var ErrWorkflowUnavailable = errors.New("workflow unavailable")

// StartWorkflow begins a workflow for a session: it snapshots the definition,
// opens a hidden CHILD session for the steps to run in, records the execution
// and starts the first step. input is the brief — what this execution is
// about, written by the agent that asked for it. The steps run off the
// conversation and share the child session with each other; the result comes
// back through a wake-up — see README invariant 30.
func (r *Runner) StartWorkflow(ctx context.Context, workflowID, parentSessionID, input string) (*store.WorkflowRun, error) {
	if r.Deps.Workflows == nil || r.Deps.WorkflowRuns == nil {
		return nil, errors.New("workflows are not wired")
	}
	wf, err := r.Deps.Workflows.Get(ctx, workflowID)
	if err != nil {
		return nil, err
	}
	if len(wf.Steps) == 0 {
		return nil, fmt.Errorf("%w: %q has no steps", ErrWorkflowUnavailable, wf.Name)
	}
	parent, err := r.Deps.Sessions.Get(ctx, parentSessionID)
	if err != nil {
		return nil, err
	}
	// Every step's agent is checked up front: finding out at step 4 that its
	// agent was deleted would leave the sequence half-run with no way to finish.
	for i := range wf.Steps {
		if _, err := r.Deps.AgentConfigs.Get(ctx, wf.Steps[i].AgentConfigID); err != nil {
			return nil, fmt.Errorf("%w: step %d (%s) names no agent", ErrWorkflowUnavailable, i+1, wf.Steps[i].Name)
		}
	}
	if err := r.checkBackgroundBudget(ctx, parentSessionID); err != nil {
		return nil, err
	}

	// The configuration the RESULT comes back under, snapshotted from the run
	// whose start_workflow call this is — the agent that asked. A start whose
	// run is no longer on the hub falls back to the session's own binding.
	originRunID := tasks.ParentRunID(ctx)
	originInherit := store.Inherit{AgentConfigID: parent.AgentConfigID, SandboxID: parent.SandboxID, WorkDir: parent.WorkDir}
	if originRunID != "" {
		if info, ok := r.hub.Info(originRunID); ok && info.AgentConfigID != "" {
			originInherit = store.Inherit{AgentConfigID: info.AgentConfigID, SandboxID: info.SandboxID, WorkDir: info.WorkDir}
		}
	}

	// The child session inherits the parent's sandbox by CARRYING the pair into
	// the first run, which CAS-binds it — the same path a task's session takes;
	// nothing writes a binding onto the row directly. Its agent is the first
	// step's, because a compaction pass between steps summarizes with the
	// SESSION's agent and a hidden session nobody picked one for has none.
	child := &store.Session{ID: store.NewID(), Name: wf.Name, Hidden: true, AgentConfigID: wf.Steps[0].AgentConfigID}
	if err := r.Deps.Sessions.Create(ctx, child); err != nil {
		return nil, fmt.Errorf("opening the workflow's session: %w", err)
	}

	first := wf.Steps[0]
	wr := &store.WorkflowRun{
		WorkflowID:      wf.ID,
		ParentSessionID: parentSessionID,
		ChildSessionID:  child.ID,
		Name:            wf.Name,
		Steps:           wf.Steps, // the snapshot: editing the definition must not steer a run in flight
		Input:           input,
		StepID:          first.ID,
		RunID:           store.NewID(), // claimed before the launch, so the row is the authority on which run is current
		// The turn whose start_workflow call this is — what the wake-up's trace
		// nests under — and the configuration the result comes back through,
		// both frozen from the run that asked (invariant 32).
		OriginRunID: originRunID,
		Inherit:     string(store.EncodeInherit(originInherit)),
		Status:      store.WorkflowRunning,
	}
	wr.StepRuns = wr.StepRuns.With(wr.StepID, wr.RunID)
	if err := r.Deps.WorkflowRuns.Create(ctx, wr); err != nil {
		// The child session was created a step above; without the execution row
		// it is an unreachable hidden orphan. Best-effort remove it so a failed
		// start leaves nothing behind.
		if delErr := r.Deps.Sessions.Delete(ctx, child.ID); delErr != nil {
			zerolog.Ctx(ctx).Warn().Err(delErr).Str("child_session_id", child.ID).
				Msg("workflow: cleaning up the child session after a failed start")
		}
		return nil, err
	}
	if err := r.startWorkflowStep(wr, first, parent, ""); err != nil {
		r.finishWorkflow(ctx, wr, wr.RunID, store.WorkflowFailed, err.Error(), "")
		// The only caller is the start_workflow tool, so this failure goes back
		// in the tool output — the model already has it in hand, and the debt
		// finishWorkflow just recorded would only repeat it later.
		(Waker{r}).Cancel(ctx, WakeKindWorkflow, wr.ID, "")
		return nil, err
	}
	return wr, nil
}

// checkBackgroundBudget refuses when the session already has as much background
// work in flight as it is allowed — workflows and tasks share one budget
// (counting them apart would let one kind hide behind the other).
func (r *Runner) checkBackgroundBudget(ctx context.Context, parentSessionID string) error {
	live, err := r.Deps.WorkflowRuns.CountLive(ctx, parentSessionID)
	if err != nil {
		return err
	}
	if r.Deps.Tasks != nil {
		running, err := r.Deps.Tasks.ListNonTerminalByParent(ctx, parentSessionID)
		if err != nil {
			return err
		}
		live += len(running)
	}
	if live >= r.hub.maxTasks {
		return fmt.Errorf("%w: this session already has %d pieces of background work running", ErrWorkflowUnavailable, live)
	}
	return nil
}

// startWorkflowStep launches one step under the run id the row already claimed.
// The sandbox comes from the PARENT session — every step shares one working
// directory with the conversation that asked. afterFailure is the error of the
// step this one is handling (empty otherwise); it leads the turn, because a
// failed run leaves no usable account of itself in the transcript.
func (r *Runner) startWorkflowStep(wr *store.WorkflowRun, step store.WorkflowStep, parent *store.Session, afterFailure string) error {
	if step.CompactBefore {
		// Best effort, before the launch while the child session is idle.
		ctx := r.hub.rootCtx
		if _, _, _, err := r.CompactSession(ctx, wr.ChildSessionID); err != nil {
			zerolog.Ctx(ctx).Info().Err(err).Str("workflow_run_id", wr.ID).Str("step_id", step.ID).
				Msg("workflow: compact before step did not run")
		}
	}
	prompt := wr.StepPrompt(step)
	if afterFailure != "" {
		prompt = "The previous step failed: " + afterFailure + "\n\n" + prompt
	}
	_, err := r.startRunWithID(wr.RunID, wr.ChildSessionID, step.AgentConfigID,
		parent.SandboxID, parent.WorkDir, prompt, "", nil, nil)
	if err == nil {
		// A stop that landed between the row moving to this run and the launch
		// registering found nothing to cancel — its hub.Cancel missed a run that
		// did not exist yet. Re-read and finish the job for it: the row is
		// terminal, so this launch is the only thing left running.
		if cur, gerr := r.Deps.WorkflowRuns.Get(r.hub.rootCtx, wr.ID); gerr == nil && cur.Status != store.WorkflowRunning {
			r.hub.Cancel(wr.RunID)
			return nil
		}
		r.publishWorkflowUpdate(wr.ParentSessionID, wr.ID, wr.RunID, store.WorkflowRunning)
	}
	return err
}

// publishWorkflowUpdate nudges the parent session's subscribers that this
// execution moved. It rides the STEP run's stream — there is no live parent
// run to carry a session event; every connection is attached to every run.
// The client refetches; the payload only routes the nudge.
func (r *Runner) publishWorkflowUpdate(parentSessionID, workflowRunID, stepRunID, status string) {
	if parentSessionID == "" || stepRunID == "" {
		return
	}
	env, err := protocol.NewEnvelope(protocol.EventWorkflowUpdated, protocol.WorkflowUpdated{
		ParentSessionID: parentSessionID, WorkflowRunID: workflowRunID, Status: status,
	})
	if err != nil {
		return
	}
	r.hub.publish(stepRunID, env)
}

// advanceWorkflow is the driver, called for EVERY run that ends (see postRun)
// rather than through the starting call's own callback: an approval resume
// passes no callback, so hanging the advance off the starting call would lose
// the workflow at the first paused step. The lookup is by RUN id — the only
// name a finished run and the row that owns it share (invariant 31).
func (r *Runner) advanceWorkflow(ctx context.Context, runID string, result *RunOutcome) {
	if r.Deps.WorkflowRuns == nil {
		return
	}
	log := zerolog.Ctx(ctx)
	wr, err := r.Deps.WorkflowRuns.ActiveForRun(ctx, runID)
	if err != nil {
		log.Warn().Err(err).Str("run_id", runID).Msg("workflow: reading the execution")
		return
	}
	// Not a workflow's run, or a superseded attempt's late callback.
	if wr == nil {
		return
	}
	// An interrupted run is PAUSED, not finished: the approval resume reopens
	// the same run id, and its ending arrives here later. Nudge the parent so
	// the strip surfaces the decision.
	if result.Interrupted {
		r.publishWorkflowUpdate(wr.ParentSessionID, wr.ID, runID, store.WorkflowRunning)
		return
	}

	// A person stopped it: no edge is followed, whatever the definition says.
	if result.Cancelled {
		r.finishWorkflow(ctx, wr, runID, store.WorkflowCancelled, "", "")
		return
	}
	failed := result.ErrMessage != ""
	next, ok := wr.NextStep(failed)
	if !ok {
		if failed {
			// No handler for this failure. The row keeps step_id, so a retry
			// resumes from the step that failed.
			r.finishWorkflow(ctx, wr, runID, store.WorkflowFailed, result.ErrMessage, "")
			return
		}
		// The last step's output IS the deliverable the wake-up carries back.
		r.finishWorkflow(ctx, wr, runID, store.WorkflowCompleted, "", result.FinalText)
		return
	}
	// The only bound on a looping on_failure edge, counted over step RUNS.
	if len(wr.StepRuns) >= store.MaxStepRuns {
		r.finishWorkflow(ctx, wr, runID, store.WorkflowFailed,
			fmt.Sprintf("stopped after %d steps — the workflow's edges are looping", len(wr.StepRuns)), "")
		return
	}

	nextRunID := store.NewID()
	// Claim the transition BEFORE launching: the row is what makes one advancer
	// the winner, so a second caller for the same finished run finds the id
	// already moved and does nothing.
	claimed, err := r.Deps.WorkflowRuns.Advance(ctx, wr.ID, runID, next.ID, nextRunID, wr.StepRuns.With(next.ID, nextRunID))
	if err != nil {
		log.Warn().Err(err).Str("workflow_run_id", wr.ID).Msg("workflow: advancing")
		return
	}
	if !claimed {
		return
	}
	parent, err := r.Deps.Sessions.Get(ctx, wr.ParentSessionID)
	if err != nil {
		r.finishWorkflow(ctx, wr, nextRunID, store.WorkflowFailed, err.Error(), "")
		return
	}
	wr.RunID, wr.StepID = nextRunID, next.ID
	if err := r.startWorkflowStep(wr, *next, parent, result.ErrMessage); err != nil {
		r.finishWorkflow(ctx, wr, nextRunID, store.WorkflowFailed, err.Error(), "")
	}
}

// finishWorkflow writes a terminal state and, unless the person cancelled it,
// records the wake-up the parent is owed — both in one transaction (see
// WorkflowRunStore.Finish). It logs a failure rather than returning it — the
// caller is a run's teardown, which has nobody to tell.
func (r *Runner) finishWorkflow(ctx context.Context, wr *store.WorkflowRun, fromRunID, status, errMsg, result string) {
	// A cancellation never wakes the parent: the person did it and the UI
	// already shows it.
	var wakeup *store.Wakeup
	if status != store.WorkflowCancelled {
		wakeup = &store.Wakeup{
			SessionID:   wr.ParentSessionID,
			Kind:        WakeKindWorkflow,
			SourceID:    wr.ID,
			Inherit:     wr.Inherit,
			ParentRunID: wr.OriginRunID,
			Payload:     workflowWakePayload(wr, status, errMsg, result),
		}
	}
	won, err := r.Deps.WorkflowRuns.Finish(ctx, wr.ID, fromRunID, status, errMsg, result, wakeup)
	if err != nil {
		zerolog.Ctx(ctx).Warn().Err(err).Str("workflow_run_id", wr.ID).Str("status", status).
			Msg("workflow: writing the terminal state")
		return
	}
	if !won {
		return
	}
	// The sequence reached a terminal state — the strip drops it, the panel
	// keeps it. Rides the ending run's stream, same as every other nudge.
	r.publishWorkflowUpdate(wr.ParentSessionID, wr.ID, fromRunID, status)
	// The debt is durable now; try to pay it. The guard inside Drain refuses
	// while the parent is busy or paused, and the next boundary retries.
	if wakeup != nil {
		(Waker{r}).Drain(ctx, wr.ParentSessionID)
	}
}

// workflowWakePayload is what the parent's turn reads: the sequence's name and
// the deliverable. It carries the same notification prefix a task's does — the
// UI would otherwise render the injected turn as a message the PERSON typed.
func workflowWakePayload(wr *store.WorkflowRun, status, errMsg, result string) string {
	var b strings.Builder
	b.WriteString(protocol.TaskNotificationPrefix)
	fmt.Fprintf(&b, "Workflow %q %s.", wr.Name, status)
	if errMsg != "" {
		fmt.Fprintf(&b, " It stopped with: %s", errMsg)
	}
	if result != "" {
		fmt.Fprintf(&b, "\n\n%s", result)
	}
	return b.String()
}

// StopWorkflow ends the whole execution, not just the running step: stopping
// one step and letting the next start is not what a person clicking stop means.
func (r *Runner) StopWorkflow(ctx context.Context, workflowRunID string) (*store.WorkflowRun, error) {
	// The hub's root context, not the request's: the teardown must complete
	// even if the HTTP caller disconnects mid-stop — same rule as RetryTask.
	ctx = r.hub.rootCtx
	wr, err := r.Deps.WorkflowRuns.Get(ctx, workflowRunID)
	if err != nil {
		return nil, err
	}
	if wr.Status != store.WorkflowRunning {
		return wr, nil // already terminal; stopping again is a no-op
	}
	// Mark first, THEN cancel: the cancelled run's ending arrives at
	// advanceWorkflow, which finds a row no longer running and does not start
	// the next step. A cancellation owes no wake-up (nil).
	won, err := r.Deps.WorkflowRuns.Finish(ctx, wr.ID, "", store.WorkflowCancelled, "", "", nil)
	if err != nil {
		return nil, err
	}
	after, err := r.Deps.WorkflowRuns.Get(ctx, workflowRunID)
	if err != nil {
		return nil, err
	}
	// The CAS is the authority: if a concurrent step already completed/failed,
	// won is false and its outcome — and the wake-up it owes — stands. Only the
	// stop that actually cancelled tears down the run and its debt.
	if !won {
		return after, nil
	}
	// Drop the paused step's approval FIRST, so a stale tool_call_id cannot
	// resume a cancelled sequence.
	if r.Deps.PendingApprovals != nil && after.RunID != "" {
		if err := r.Deps.PendingApprovals.Delete(ctx, after.RunID); err != nil {
			zerolog.Ctx(ctx).Warn().Err(err).Str("run_id", after.RunID).Msg("workflow: clearing a stopped step's approval")
		}
	}
	(Waker{r}).Cancel(ctx, WakeKindWorkflow, wr.ID, "")
	r.hub.Cancel(after.RunID)
	// The nudge finishWorkflow's path publishes: Finish was called directly
	// here (a stop owes no wake-up), and without it every other tab's strip
	// keeps showing a running sequence.
	r.publishWorkflowUpdate(after.ParentSessionID, after.ID, after.RunID, store.WorkflowCancelled)
	return after, nil
}

// RetryWorkflow re-runs a terminal execution from the step it stopped at,
// keeping the steps that already succeeded. It executes the SNAPSHOT, so a
// definition edited since is not silently picked up mid-sequence.
func (r *Runner) RetryWorkflow(ctx context.Context, workflowRunID string) (*store.WorkflowRun, error) {
	// The hub's root context, not the request's: the run this starts outlives
	// the HTTP call, and a disconnect between the claim and the launch would
	// otherwise strand the row running with no live run.
	ctx = r.hub.rootCtx
	wr, err := r.Deps.WorkflowRuns.Get(ctx, workflowRunID)
	if err != nil {
		return nil, err
	}
	// Only a FAILED execution retries: re-running a success would repeat its
	// side effects (a deploy, a send, a charge). The store's Restart CAS
	// enforces the same predicate.
	if wr.Status != store.WorkflowFailed {
		return nil, fmt.Errorf("%w: %q is %s, and only a failed workflow can be retried", ErrWorkflowUnavailable, wr.Name, wr.Status)
	}
	idx := wr.StepIndex(wr.StepID)
	if idx < 0 {
		return nil, fmt.Errorf("%w: the step it stopped at is no longer in the snapshot", ErrWorkflowUnavailable)
	}
	// The ceiling counts step RUNS across the whole execution, retries included,
	// so a step that keeps failing cannot be retried without end.
	if len(wr.StepRuns) >= store.MaxStepRuns {
		return nil, fmt.Errorf("%w: %q has already run %d steps", ErrWorkflowUnavailable, wr.Name, len(wr.StepRuns))
	}
	// A retry is one more piece of background work on the parent — subject to
	// the same shared budget a fresh start is.
	if err := r.checkBackgroundBudget(ctx, wr.ParentSessionID); err != nil {
		return nil, err
	}
	// The child session is this execution's alone, so the only thing that can
	// be in the way is the retried step itself.
	if rid, live := r.hub.ActiveRunForSession(wr.ChildSessionID); live {
		return nil, ErrSessionBusy{RunID: rid}
	}
	parent, err := r.Deps.Sessions.Get(ctx, wr.ParentSessionID)
	if err != nil {
		return nil, err
	}
	runID := store.NewID()
	ok, err := r.Deps.WorkflowRuns.Restart(ctx, wr.ID, wr.RunID, wr.StepID, runID, wr.StepRuns.With(wr.StepID, runID))
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%w: it started again while this retry was being prepared", ErrWorkflowUnavailable)
	}
	// A retry supersedes whatever the failed attempt was owed: the outcome the
	// parent has not heard yet is about to be replaced.
	(Waker{r}).Cancel(ctx, WakeKindWorkflow, wr.ID, "")
	wr.RunID, wr.Status = runID, store.WorkflowRunning
	if err := r.startWorkflowStep(wr, wr.Steps[idx], parent, ""); err != nil {
		r.finishWorkflow(ctx, wr, runID, store.WorkflowFailed, err.Error(), "")
		return nil, err
	}
	return r.Deps.WorkflowRuns.Get(ctx, workflowRunID)
}

// FailInterruptedWorkflows is the workflow half of the restart reconciliation
// (see FailOrphanedTasks, and its ordering rule): an execution recorded as
// running has no live step after a restart, so it is failed at the step it
// reached — which a retry can resume from.
func (r *Runner) FailInterruptedWorkflows(ctx context.Context) {
	if r.Deps.WorkflowRuns == nil {
		return
	}
	running, err := r.Deps.WorkflowRuns.ListRunning(ctx)
	if err != nil {
		zerolog.Ctx(ctx).Warn().Err(err).Msg("workflow: listing running executions at startup")
		return
	}
	for i := range running {
		wr := &running[i]
		// A step PAUSED on an approval is not an orphan: the approval persists,
		// it is answerable from the parent's strip, and answering resumes the
		// run. Same rule a task's input_required follows.
		if r.Deps.PendingApprovals != nil {
			paused, err := r.Deps.PendingApprovals.ListBySession(ctx, wr.ChildSessionID)
			if err != nil {
				zerolog.Ctx(ctx).Warn().Err(err).Str("workflow_run_id", wr.ID).
					Msg("workflow: checking for a paused step; leaving it alone")
				continue
			}
			if len(paused) > 0 {
				continue
			}
		}
		// Through finishWorkflow, so the parent is owed the outcome: a sequence
		// killed by a restart is exactly the case a durable debt exists for.
		r.finishWorkflow(ctx, wr, wr.RunID, store.WorkflowFailed,
			"interrupted by a server restart", "")
	}
}

// FailWorkflowForExpiredApproval ends the execution whose step was waiting on
// an approval that has now expired. Without it the row stays running forever:
// its step will never resume, and nothing else will ever claim the ending.
func (r *Runner) FailWorkflowForExpiredApproval(ctx context.Context, childSessionID string) {
	if r.Deps.WorkflowRuns == nil {
		return
	}
	wr, err := r.Deps.WorkflowRuns.ByChildSession(ctx, childSessionID)
	if err != nil || wr == nil {
		return
	}
	r.finishWorkflow(ctx, wr, wr.RunID, store.WorkflowFailed,
		"a step's approval expired unanswered", "")
}

// isBackgroundRun reports whether this run is one nobody is sitting in front
// of. A task knows from its meta; a workflow step only from its session, since
// the step is an ordinary run started on the execution's child session. ANY
// status counts: a child session belongs to its execution for good, and a step
// launch racing a stop must still build as a background run, not pick up the
// chat toolset because the row went cancelled a moment earlier.
//
// A failed lookup is an ERROR, not a "no": reading it as a chat run hands a
// background agent plan mode, and its submit_plan approval would land in a
// session nobody can open — a sequence stuck forever on an unanswerable
// question.
func (r *Runner) isBackgroundRun(ctx context.Context, sessionID string, task *TaskMeta) (bool, error) {
	if task != nil {
		return true, nil
	}
	if r.Deps.WorkflowRuns == nil {
		return false, nil
	}
	wr, err := r.Deps.WorkflowRuns.ByChildSessionAny(ctx, sessionID)
	if err != nil {
		return false, fmt.Errorf("resolving the workflow for session %s: %w", sessionID, err)
	}
	return wr != nil, nil
}
