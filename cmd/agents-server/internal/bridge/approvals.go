package bridge

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/middleware"
	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/cmd/agents-server/internal/logging"
	"github.com/zzir/agents-go/cmd/agents-server/internal/sandboxes"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// approvalSettleTimeout bounds ResolveApproval's wait for the paused segment's
// postRun; usually already settled, it only caps a pathological stall.
const approvalSettleTimeout = 5 * time.Second

// ApprovalVoidError reports that an approval could not be applied because its
// background task reached a terminal state first (a concurrent stop or reap
// won). It is a conflict, not a server fault — handlers map it to 409.
type ApprovalVoidError struct{ TaskID string }

func (e *ApprovalVoidError) Error() string {
	return "task " + e.TaskID + " is no longer awaiting approval; the decision is void"
}

// ApprovalNotReadyError reports that the paused run had not finished settling
// into an approvable state by the time the decision arrived. The pending row is
// preserved, so the decision can simply be retried. Handlers map it to 409.
type ApprovalNotReadyError struct{ RunID string }

func (e *ApprovalNotReadyError) Error() string {
	return "run " + e.RunID + " is not yet ready for an approval decision; retry"
}

// StaleApprovalAttemptError reports an approval whose attempt is no longer
// the task's current one: the task was retried past the run that paused, so
// the decision has nothing left to resume. The row is discarded — restoring
// it would refuse forever — and the current attempt is untouched. Handlers
// map it to 409.
type StaleApprovalAttemptError struct {
	TaskID        string
	ApprovalRunID string
	CurrentRunID  string
}

func (e *StaleApprovalAttemptError) Error() string {
	return "task " + e.TaskID + " was retried past the paused run " + e.ApprovalRunID +
		" (now on " + e.CurrentRunID + "); the approval is stale and was discarded"
}

// persistInterruption writes an interrupted run's SDK state and pending calls
// BEFORE the pause is announced; a failure fails the segment instead.
func (r *Runner) persistInterruption(result *RunOutcome) error {
	if r.Deps.PendingApprovals == nil || result == nil || !result.Interrupted || result.SDKState == nil {
		return nil
	}
	stateJSON, err := result.SDKState.MarshalJSON()
	if err != nil {
		return fmt.Errorf("serializing run state: %w", err)
	}
	calls := make([]store.PendingToolCall, 0, len(result.Interruptions))
	for _, item := range result.Interruptions {
		calls = append(calls, store.PendingToolCall{
			ToolCallID: item.CallID,
			ToolName:   item.ToolName,
			Arguments:  item.Arguments,
		})
	}
	callsJSON, _ := json.Marshal(calls)
	approval := &store.PendingApproval{
		RunID:         result.RunID,
		SessionID:     result.SessionID,
		AgentConfigID: result.AgentConfigID,
		ProjectID:     result.ProjectID,
		State:         string(stateJSON),
		ToolCalls:     callsJSON,
		// The paused turn's user text, so the UI rebuilds the bubble on reload —
		// the SDK writes the turn only once it completes.
		UserInput: session.UserText(result.SDKState.UserInput),
	}
	// A task's run pauses its TASK in the same write (TaskStore.Pause) —
	// invariant 37; the manager's later mark finds the row already paused.
	if info, ok := r.hub.Info(result.RunID); ok && info.Task != nil && info.Task.TaskID != "" && r.Deps.Tasks != nil {
		won, err := r.Deps.Tasks.Pause(context.Background(), info.Task.TaskID, result.RunID, nil, approval)
		if err != nil {
			return err
		}
		if !won {
			return fmt.Errorf("task %s is no longer working on run %s", info.Task.TaskID, result.RunID)
		}
		return nil
	}
	return r.Deps.PendingApprovals.Save(context.Background(), approval)
}

// buildAgentRegistry builds the agent and its reachable handoff targets by name for
// agents.RunStateFromJSON; pass the run's project, or the re-run agent loses its tools.
func (r *Runner) buildAgentRegistry(ctx context.Context, agentConfigID, projectID string, background bool, ownerID string) (map[string]*agents.Agent, *BuildResult, error) {
	built, err := buildFullAgent(ctx, r.Deps, agentConfigID, projectID, background, ownerID)
	if err != nil {
		return nil, nil, err
	}
	registry := map[string]*agents.Agent{}
	var walk func(a *agents.Agent)
	walk = func(a *agents.Agent) {
		if a == nil || registry[a.Name] != nil {
			return
		}
		registry[a.Name] = a
		for _, ho := range a.Handoffs {
			// Target is the static declaration HandoffTo fills; a dynamic
			// handoff (nil) cannot be enumerated without user code, so it is skipped.
			walk(ho.Target)
		}
	}
	walk(built.Agent)
	return registry, built, nil
}

// planUnlockPersistTimeout bounds the marker write: detached from the run's
// cancellation but not unbounded — the SQLite pool is one connection.
const planUnlockPersistTimeout = 10 * time.Second

// errResumeStopped is the verify hook's refusal: the work was stopped between
// the claim and the launch, so the resumed run must not start.
var errResumeStopped = errors.New("the work was stopped before the approval could resume it")

// armPlanUnlock makes clearing the session's planning column the PRECONDITION
// of the first unlock: a failed write fails submit_plan and the review repeats.
func armPlanUnlock(phase *middleware.PlanPhase, sa *store.EntryStore, ref session.Ref) {
	if phase == nil {
		return
	}
	phase.OnUnlock(func() error {
		ctx, cancel := context.WithTimeout(context.Background(), planUnlockPersistTimeout)
		defer cancel()
		// Clearing the column IS the durable record of the approval —
		// invariant 33. Idempotent, so a replayed unlock is a no-op.
		if err := sa.SetSessionPlanning(ctx, ref, false); err != nil {
			return fmt.Errorf("persisting the plan-unlock record: %w", err)
		}
		return nil
	})
}

// ApplyPlanIntent records what a run request asked of the session's plan
// phase, before the run starts — the only way in (workbench invariant 33). A
// nil intent leaves the phase alone; one that already matches writes nothing.
func (r *Runner) ApplyPlanIntent(ctx context.Context, sessionID string, plan *bool) error {
	if plan == nil {
		return nil
	}
	ref, err := store.RefFor(ctx, r.db, sessionID)
	if err != nil {
		return err
	}
	sa := store.NewEntryStoreFor(r.db, ref)
	planning, err := sa.SessionIsPlanning(ctx, ref)
	if err != nil {
		return err
	}
	if planning == *plan {
		return nil
	}
	return sa.SetSessionPlanning(ctx, ref, *plan)
}

// restorePlanPhase puts a plan-mode run into the SESSION's phase (invariant 33)
// and arms the unlock. A read failure is an error: nothing is claimed yet.
func (r *Runner) restorePlanPhase(ctx context.Context, phase *middleware.PlanPhase, sa *store.EntryStore, ref session.Ref) error {
	if phase == nil {
		return nil
	}
	planning, err := sa.SessionIsPlanning(ctx, ref)
	if err != nil {
		return fmt.Errorf("reading plan-unlock marker: %w", err)
	}
	if !planning {
		// No hook is armed yet, so this cannot fail — arming AFTER is also
		// what keeps a replayed unlock from writing the column twice.
		_ = phase.Unlock()
	}
	armPlanUnlock(phase, sa, ref)
	return nil
}

// ResolveApproval applies an approve/reject decision to the pending tool call
// and launches the run's continuation under the same run id. It loads the
// persisted RunState (so it works after a restart and from any transport),
// deletes the pending record, and resumes via the hub. onDone fires when the
// continuation terminates (e.g. to persist a further interruption).
// It also returns the paused session's id (whenever the pending row was loaded,
// even on a later error) so a failed decision stays attributable to its session.
func (r *Runner) ResolveApproval(ctx context.Context, toolCallID string, approve bool, scope ApprovalScope, reason string, onDone func(*RunOutcome)) (runID, sessionID string, err error) {
	if r.Deps.PendingApprovals == nil {
		return "", "", errors.New("approvals are not persisted")
	}
	pending, _, err := r.Deps.PendingApprovals.FindByToolCall(ctx, toolCallID)
	if err != nil {
		return "", "", err
	}

	// Once the pending row is deleted, bailing out half-done strands the run:
	// every MUTATION below runs detached from the request's cancellation; reads do not.
	mctx := context.WithoutCancel(ctx)

	// A workflow step waiting to START has no run to resume: the decision
	// starts the step's run or ends the execution.
	if pending.Kind == store.ApprovalKindStep {
		runID, err = r.resolveStepApproval(ctx, pending, approve)
		return runID, pending.SessionID, err
	}

	// A RunState outside the SDK's decode window is discarded (else every retry
	// 500s); the check is the SDK's window, so an additive bump still resumes.
	if v := pendingStateSchemaVersion(pending.State); !agents.RunStateVersionSupported(v) {
		if delErr := r.Deps.PendingApprovals.Delete(mctx, pending.RunID); delErr != nil {
			logging.Ctx(ctx).Error("discarding stale pending approval", "error", delErr, "run_id", pending.RunID)
		}
		return "", pending.SessionID, &StaleApprovalStateError{RunID: pending.RunID, HaveVersion: v, WantVersion: agents.RunStateSchemaVersion}
	}

	// A pending approval may belong to a background task's child session — its
	// agent must be rebuilt task-shaped (no task tools), like the original run.
	taskMeta, err := r.taskMeta(ctx, pending.SessionID)
	if err != nil {
		// Rebuilding a task run as a chat run would hand it the task tools and
		// skip its reclaim; refuse rather than guess.
		return "", pending.SessionID, err
	}
	// The rebuild carries the owner's role like the original build did.
	sess, err := r.Deps.Sessions.Get(ctx, pending.SessionID)
	if err != nil {
		return "", pending.SessionID, err
	}
	registry, rebuilt, err := r.buildAgentRegistry(ctx, pending.AgentConfigID, pending.ProjectID, taskMeta != nil, sess.OwnerID)
	if err != nil {
		return "", pending.SessionID, fmt.Errorf("rebuilding agent: %w", err)
	}
	// The rebuilt agent IS the resumed run's executor, so its sandbox reference
	// lives as long as that run: handed to onDone below, else released here.
	handedOff := false
	defer func() {
		if !handedOff {
			rebuilt.Release()
		}
	}()
	state, err := agents.RunStateFromJSON([]byte(pending.State), registry)
	if err != nil {
		return "", pending.SessionID, fmt.Errorf("restoring run state: %w", err)
	}
	// Restore the phase from the session's column (invariant 33), or a pause
	// after the plan ended resumes without write tools; a failed read retries.
	resumeRef, refErr := store.RefFor(ctx, r.db, pending.SessionID)
	if refErr != nil {
		return "", pending.SessionID, fmt.Errorf("resolving session for plan phase: %w", refErr)
	}
	resumeStore := store.NewEntryStoreFor(r.db, resumeRef)
	resumeStore.SetRunID(pending.RunID)
	if err := r.restorePlanPhase(ctx, rebuilt.PlanPhase, resumeStore, resumeRef); err != nil {
		return "", pending.SessionID, err
	}

	item := findApprovalItem(state, toolCallID)
	if item == nil {
		return "", pending.SessionID, fmt.Errorf("tool call %s not found in run state", toolCallID)
	}
	if approve {
		state.Approve(item, false)
	} else {
		state.Reject(item, false, reason)
	}

	// Wait for the paused segment's postRun (it marks the task input_required,
	// THEN closes the gate) so ReclaimWorking finds the row it expects.
	r.hub.waitDone(pending.RunID, time.Now().Add(approvalSettleTimeout))

	// Deleting the record is the exclusive claim, and must precede the resume.
	// For a task the claim and the row's CAS are ONE write — invariant 23.
	if taskMeta != nil && taskMeta.TaskID != "" {
		outcome, cerr := r.Deps.Tasks.ClaimApprovalWorking(mctx, taskMeta.TaskID, pending.RunID)
		if cerr != nil {
			return "", pending.SessionID, fmt.Errorf("reclaiming task %s: %w", taskMeta.TaskID, cerr)
		}
		switch outcome {
		case store.ClaimTaken:
			return "", pending.SessionID, fmt.Errorf("claiming pending approval: %w", store.ErrNotFound)
		case store.ClaimTaskNotPaused:
			// Terminal: void. A different attempt: stale (the row stays and
			// refuses). Still this attempt, not paused yet: not ready, retry.
			cur, gerr := r.Deps.Tasks.Get(mctx, taskMeta.TaskID)
			if gerr == nil && !isTerminalTaskStatus(cur.Status) {
				if cur.RunID != pending.RunID {
					return "", pending.SessionID, &StaleApprovalAttemptError{TaskID: taskMeta.TaskID, ApprovalRunID: pending.RunID, CurrentRunID: cur.RunID}
				}
				return "", pending.SessionID, &ApprovalNotReadyError{RunID: pending.RunID}
			}
			return "", pending.SessionID, &ApprovalVoidError{TaskID: taskMeta.TaskID}
		}
	} else if err := r.Deps.PendingApprovals.Delete(mctx, pending.RunID); err != nil {
		return "", pending.SessionID, fmt.Errorf("claiming pending approval: %w", err)
	}

	// The continuation reopens the SAME run id. The wrapped onDone releases the
	// rebuild's sandbox reference when the resumed segment ends.
	resumeDone := func(res *RunOutcome) {
		rebuilt.Release()
		if onDone != nil {
			onDone(res)
		}
	}
	// verify runs after the run registers but BEFORE its goroutine launches:
	// a stop that finalized the task meanwhile means the tool must not run.
	verify := func() error {
		if taskMeta != nil && taskMeta.TaskID != "" {
			if cur, gerr := r.Deps.Tasks.Get(mctx, taskMeta.TaskID); gerr == nil && isTerminalTaskStatus(cur.Status) {
				return errResumeStopped
			}
		}
		// Standing trust (same_command, all) is written HERE: after the claim
		// held, before the launch, so the loser of two decisions widens nothing.
		if approve {
			r.applyCommandTrust(scope, item, trustSessionID(pending.SessionID, taskMeta))
		}
		return nil
	}
	runID, err = r.ResumeRun(pending.RunID, state, pending.SessionID, pending.AgentConfigID, pending.ProjectID, verify, resumeDone)
	if errors.Is(err, errResumeStopped) {
		// Stopped between the claim and the launch: nothing ran, nothing to
		// restore. A 409 like a terminal run's, not a 500.
		return "", pending.SessionID, ErrRunNotResumable{RunID: pending.RunID, Status: RunCancelled}
	}
	if err != nil {
		// Give the approval back so the decision can be retried; for a task the
		// row and its input_required go back in ONE write (Pause).
		if taskMeta != nil && taskMeta.TaskID != "" {
			if _, perr := r.Deps.Tasks.Pause(mctx, taskMeta.TaskID, pending.RunID, nil, pending); perr != nil {
				logging.Ctx(ctx).Error("restoring the paused task after a failed resume", "error", perr, "task_id", taskMeta.TaskID)
			}
		} else {
			r.restorePendingApproval(mctx, pending)
		}
		return "", pending.SessionID, err
	}
	handedOff = true
	return runID, pending.SessionID, nil
}

// restorePendingApproval writes a claimed row back after a failed claim/resume
// (pre-decision state, so a retry re-applies cleanly). Detached context.
func (r *Runner) restorePendingApproval(ctx context.Context, pending *store.PendingApproval) {
	if saveErr := r.Deps.PendingApprovals.Save(context.WithoutCancel(ctx), pending); saveErr != nil {
		logging.Ctx(ctx).Error("restoring pending approval after failed claim/resume", "error", saveErr, "run_id", pending.RunID)
	}
}

// StaleApprovalStateError is returned when a persisted RunState cannot be
// resumed because it was written by an older server binary — its schema version
// no longer matches the current one. The stale record is discarded before this
// is returned, so the caller should re-initiate the run rather than retry.
type StaleApprovalStateError struct {
	RunID       string
	HaveVersion string
	WantVersion string
}

func (e *StaleApprovalStateError) Error() string {
	have := e.HaveVersion
	have = cmp.Or(have, "unknown")
	return fmt.Sprintf("paused run %s predates the current server version (state schema %s, want %s) and cannot be resumed — re-initiate the run",
		e.RunID, have, e.WantVersion)
}

// pendingStateSchemaVersion reads just the schema_version field of a serialized
// RunState, so a version mismatch is detected without a full (failing) decode.
func pendingStateSchemaVersion(stateJSON string) string {
	var probe struct {
		SchemaVersion string `json:"schema_version"`
	}
	_ = json.Unmarshal([]byte(stateJSON), &probe)
	return probe.SchemaVersion
}

// findApprovalItem returns the interruption in state matching callID, or nil.
func findApprovalItem(state *agents.RunState, callID string) *agents.ToolApprovalItem {
	for _, item := range state.Interruptions {
		if item.CallID == callID {
			return item
		}
	}
	return nil
}

// ApprovalScope controls how far an approve decision extends for exec_command:
// once = just this call; same = trust this exact command for the rest of the
// session; all = trust every command for the session. Ignored for other tools.
type ApprovalScope string

// Approval scopes for ResolveApproval — how far an approve decision extends.
const (
	ApprovalOnce        ApprovalScope = "once"
	ApprovalSameCommand ApprovalScope = "same"
	ApprovalAll         ApprovalScope = "all"
)

// ParseApprovalScope maps a client scope string to an ApprovalScope, defaulting
// to once for an empty or unknown value.
func ParseApprovalScope(s string) ApprovalScope {
	switch ApprovalScope(s) {
	case ApprovalSameCommand:
		return ApprovalSameCommand
	case ApprovalAll:
		return ApprovalAll
	default:
		return ApprovalOnce
	}
}

// execCommandToolName is the fixed name of the sandbox shell tool whose
// executions carry per-session command-trust grants.
const execCommandToolName = "exec_command"

// applyCommandTrust records a session-level exec_command grant per scope; a
// no-op for other tools, an empty session, or the once scope.
func (r *Runner) applyCommandTrust(scope ApprovalScope, item *agents.ToolApprovalItem, sessionID string) {
	if item.ToolName != execCommandToolName || sessionID == "" || r.Deps.SandboxManager == nil {
		return
	}
	trust := r.Deps.SandboxManager.Trust().ForSession(sessionID)
	switch scope {
	case ApprovalSameCommand:
		trust.AllowCommand(sandboxes.CommandHash(item.Arguments))
	case ApprovalAll:
		trust.AllowAll()
	}
}
