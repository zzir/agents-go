package bridge

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/rs/zerolog"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/middleware"
	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// approvalSettleTimeout bounds how long ResolveApproval waits for the paused
// run segment's teardown (postRun) to finish before claiming the approval. In
// the common case the segment settled long ago and the wait returns at once;
// the bound only caps a pathological stall so an approve never hangs.
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

// persistInterruption serializes an interrupted run's SDK state and its
// pending tool calls to the store, so the approval survives a restart and can
// be resumed from any connection. It runs BEFORE the pause is announced: a
// failure fails the segment instead (finishResult) — a pause with no row is a
// decision nobody can make.
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
		SandboxID:     result.SandboxID,
		WorkDir:       result.WorkDir,
		State:         string(stateJSON),
		ToolCalls:     callsJSON,
		// The user-authored text of the paused turn's new input, so the UI can
		// rebuild the user bubble on reload — the SDK only writes the turn to
		// the session once it completes.
		UserInput: session.UserText(result.SDKState.UserInput),
	}
	// A task's run pauses its TASK too, in the same write: the approval filed
	// and the row input_required together (store.TaskStore.Pause), so no
	// failure between them can leave an approval nobody can act on because
	// the task still says working. The manager's own mark, when the run's
	// ending is reported to it, then finds the row already paused.
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

// buildAgentRegistry builds the agent from its config and returns a name→agent
// registry covering it and all reachable handoff targets, as required by
// agents.RunStateFromJSON. It must build with the run's sandboxID: the
// restored state's CurrentAgent is resolved FROM this registry and is the very
// agent the SDK re-runs, so omitting the sandbox here strips its
// sandbox-backed tools (exec_command, read_file, …) and the approved call
// fails with "tool not found on agent".
func (r *Runner) buildAgentRegistry(ctx context.Context, agentConfigID, sandboxID, workDir string, background bool) (map[string]*agents.Agent, *BuildResult, error) {
	built, err := buildFullAgent(ctx, r.Deps, agentConfigID, sandboxID, workDir, background)
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
			// Target is the static declaration HandoffTo fills; the bridge
			// builds every handoff that way. A dynamic handoff (Target nil)
			// cannot be enumerated without invoking user code, so it is
			// skipped — nil is how it declares that.
			walk(ho.Target)
		}
	}
	walk(built.Agent)
	return registry, built, nil
}

// planUnlockPersistTimeout bounds the marker write. The hook must be
// detached from the run's cancellation (a client disconnect must not decide
// whether the mark lands) but NOT unbounded: the SQLite pool is a single
// connection, and an unbounded wait on it would wedge submit_plan — and the
// run's own cancel — behind whatever holds the connection.
const planUnlockPersistTimeout = 10 * time.Second

// errResumeStopped is the verify hook's refusal: the task or workflow this
// approval belonged to was stopped between the claim and the launch, so the
// resumed run must not start. It is not a failure to restore from — the work is
// terminal and the approval is void.
var errResumeStopped = errors.New("the work was stopped before the approval could resume it")

// armPlanUnlock makes clearing the session's planning column the PRECONDITION
// of the phase's first unlock: the hook's error fails the unlock, so the run is
// never executing ahead of its durable record — a failed write surfaces as a
// submit_plan tool error and the review repeats.
func armPlanUnlock(phase *middleware.PlanPhase, sa *store.EntryStore, ref session.Ref) {
	if phase == nil {
		return
	}
	phase.OnUnlock(func() error {
		ctx, cancel := context.WithTimeout(context.Background(), planUnlockPersistTimeout)
		defer cancel()
		// Clearing the session's plan phase IS the durable record that the plan
		// was approved; persisting it is the precondition for the run leaving
		// the planning phase. Idempotent, so a replayed unlock is a no-op.
		if err := sa.SetSessionPlanning(ctx, ref, false); err != nil {
			return fmt.Errorf("persisting the plan-unlock record: %w", err)
		}
		return nil
	})
}

// ApplyPlanIntent records what a run request asked of the session's plan phase,
// before the run starts. It is the ONLY way in: plan mode is a restraint, so a
// person turns it on with the message it applies to, which also makes the two
// atomic — setting the phase and starting the run cannot interleave with
// another run any more.
//
// A nil intent leaves the phase alone (a client that knows nothing about plan
// mode cannot knock a session out of it), and an intent that already matches
// writes nothing — the markers are entries, and one per turn would be noise.
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

// restorePlanPhase puts a plan-mode run into the phase the SESSION's column
// says it is in, and arms the unlock this run may perform. Every run consults
// it, fresh or resumed: a plan approved in one turn is not re-asked in the next.
//
// A READ failure is an error, not a warning: nothing has been claimed yet, so
// failing here leaves a pending approval intact for a retry — silently resuming
// in the planning phase would strip a mid-execution run of its write tools.
//
// The column stays the truth even though agents.RunState.Extra could carry the
// same bit: it is written the moment the unlock EXECUTES (OnUnlock persists as a
// precondition), so it survives a crash with no pause — a window Extra, written
// only when a pause serializes state, cannot cover.
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

	// The claim and the resume that follows form a multi-step migration across
	// two tables plus the hub. Once the pending row is deleted, bailing out
	// half-done would strand the paused run — so every MUTATION below runs on a
	// context detached from the request's cancellation: a client that
	// disconnects mid-approve no longer aborts the migration in the middle.
	// Reads still use the request ctx; a disconnect there aborts before any
	// state changed, which is safe to retry.
	mctx := context.WithoutCancel(ctx)

	// A workflow step waiting to START: there is no run to resume, so none of
	// the run-state machinery below applies — the decision starts the step's
	// run or ends the execution.
	if pending.Kind == store.ApprovalKindStep {
		runID, err = r.resolveStepApproval(ctx, pending, approve)
		return runID, pending.SessionID, err
	}

	// A RunState outside the SDK's decode window can never be resumed. Detect
	// that up front so it surfaces as a clear, actionable error instead of a
	// masked 500, and discard the stale row so it stops wedging the session (a
	// masked 500 on every approve/reject retry, row never deleted). The check
	// is the SDK's own window — same major, minor within what it still
	// decodes — NOT string equality: an equality check here would destroy
	// states a purely additive SDK bump (1.5 → 1.6) resumes fine.
	if v := pendingStateSchemaVersion(pending.State); !agents.RunStateVersionSupported(v) {
		if delErr := r.Deps.PendingApprovals.Delete(mctx, pending.RunID); delErr != nil {
			zerolog.Ctx(ctx).Error().Err(delErr).Str("run_id", pending.RunID).Msg("discarding stale pending approval")
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
	registry, rebuilt, err := r.buildAgentRegistry(ctx, pending.AgentConfigID, pending.SandboxID, pending.WorkDir, taskMeta != nil)
	if err != nil {
		return "", pending.SessionID, fmt.Errorf("rebuilding agent: %w", err)
	}
	// The rebuilt agent IS the resumed run's executor (the state resolves its
	// CurrentAgent from this registry), so the build's sandbox reference must
	// live exactly as long as that run. Handed off to the resume's onDone
	// below; every earlier exit releases it here — Release is idempotent, so
	// the belt covers a path that both hands off and fails.
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
	// A plan-mode rebuild starts in the planning phase, but this run may have
	// moved past it: without the unlock, a pause AFTER the plan phase ended
	// (an exec_command approval, say) would resume into a run whose write
	// tools had vanished again. The durable truth is the session's planning
	// column, cleared the moment the approved submit_plan EXECUTED. A failed
	// read aborts the resolve — the pending approval is still unclaimed at this
	// point, so the decision simply retries.
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

	// Wait for the paused segment's teardown to complete before claiming. The
	// run goroutine's postRun marks a task input_required (working ->
	// input_required) and only THEN closes the segment's done gate, so waiting
	// on it guarantees the task row is already in the state ReclaimWorking
	// expects — otherwise a fast approve races ahead of postRun, fails
	// ReclaimWorking (task still "working"), and with the row gone strands the
	// approval forever.
	r.hub.waitDone(pending.RunID, time.Now().Add(approvalSettleTimeout))

	// Deleting the record is the exclusive claim on this approval vs. a
	// concurrent approve: of two racing decisions exactly one proceeds. It
	// must precede the resume — the continuation may itself interrupt and
	// persist a fresh record. For a task's approval the claim and the row's
	// CAS (input_required -> working, bound to THIS attempt: an approval that
	// outlived its attempt must not reclaim the run that replaced its own)
	// are ONE write — the task never ends up working with an approval left,
	// nor answered while it stays paused, and a claim that does not hold
	// writes nothing, so nothing needs putting back.
	if taskMeta != nil && taskMeta.TaskID != "" {
		outcome, cerr := r.Deps.Tasks.ClaimApprovalWorking(mctx, taskMeta.TaskID, pending.RunID)
		if cerr != nil {
			return "", pending.SessionID, fmt.Errorf("reclaiming task %s: %w", taskMeta.TaskID, cerr)
		}
		switch outcome {
		case store.ClaimTaken:
			return "", pending.SessionID, fmt.Errorf("claiming pending approval: %w", store.ErrNotFound)
		case store.ClaimTaskNotPaused:
			// Terminal (a stop/reap won — the decision is void; nothing may
			// revive a cancelled task); a DIFFERENT attempt (a retry moved
			// the task past this approval's run — stale, and it stays as a
			// row that will refuse); or still this attempt and not paused yet
			// (the settle wait should have prevented this) — not ready, the
			// row untouched for the retry of the decision.
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

	// The continuation reopens the SAME run id, so the whole turn — both the
	// interrupted and resumed halves — shares one event stream and trace group.
	// The wrapped onDone releases the rebuild's sandbox reference when the
	// resumed segment ends (completion, error, cancel or a further interrupt —
	// the next resume performs its own rebuild and acquire).
	resumeDone := func(res *RunOutcome) {
		rebuilt.Release()
		if onDone != nil {
			onDone(res)
		}
	}
	// verify runs after the run registers but BEFORE its goroutine launches: a
	// stop that finalized the task between our claim and here means the
	// approved tool must not run at all. Checking after the launch (as before)
	// let the tool fire and cause a side effect before the cancel could land.
	verify := func() error {
		if taskMeta != nil && taskMeta.TaskID != "" {
			if cur, gerr := r.Deps.Tasks.Get(mctx, taskMeta.TaskID); gerr == nil && isTerminalTaskStatus(cur.Status) {
				return errResumeStopped
			}
		}
		// The standing trust a scope grants (same_command, all) is written
		// HERE: after the exclusive claim and the task's CAS held, and before
		// the continuation launches — so the run's very next exec_command
		// already reads it, and the loser of two racing decisions never
		// widened anything. The approved call itself needs no trust: its
		// decision is in the run state.
		if approve {
			r.applyCommandTrust(scope, item, trustSessionID(pending.SessionID, taskMeta))
		}
		return nil
	}
	runID, err = r.ResumeRun(pending.RunID, state, pending.SessionID, pending.AgentConfigID, pending.SandboxID, pending.WorkDir, verify, resumeDone)
	if errors.Is(err, errResumeStopped) {
		// The work was stopped in the window between the claim and the launch.
		// The approval is void and the run never started — nothing to restore,
		// nothing executed. Surfaced as the same 409 a terminal run gives, not a
		// 500: it is a state conflict, not a fault.
		return "", pending.SessionID, ErrRunNotResumable{RunID: pending.RunID, Status: RunCancelled}
	}
	if err != nil {
		// Give the approval back (e.g. the session has a live run right now) so
		// the decision can be retried once the session frees up — losing the row
		// here would strand the paused run forever. For a task's run the row
		// and the task's input_required go back in ONE write (Pause), never
		// one without the other.
		if taskMeta != nil && taskMeta.TaskID != "" {
			if _, perr := r.Deps.Tasks.Pause(mctx, taskMeta.TaskID, pending.RunID, nil, pending); perr != nil {
				zerolog.Ctx(ctx).Error().Err(perr).Str("task_id", taskMeta.TaskID).Msg("restoring the paused task after a failed resume")
			}
		} else {
			r.restorePendingApproval(mctx, pending)
		}
		return "", pending.SessionID, err
	}
	handedOff = true
	return runID, pending.SessionID, nil
}

// restorePendingApproval writes a claimed pending-approval row back after a
// failed claim/resume, so a paused run is never stranded by a lost row. The
// row's serialized state is the original (pre-decision) one, so a retry
// re-applies the decision cleanly. Detached context — the restore must land
// even if the request that triggered it is gone.
func (r *Runner) restorePendingApproval(ctx context.Context, pending *store.PendingApproval) {
	if saveErr := r.Deps.PendingApprovals.Save(context.WithoutCancel(ctx), pending); saveErr != nil {
		zerolog.Ctx(ctx).Error().Err(saveErr).Str("run_id", pending.RunID).
			Msg("restoring pending approval after failed claim/resume")
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

// applyCommandTrust records a session-level exec_command grant per the approval
// scope. It is a no-op for non-exec_command tools, an empty session, or the
// once scope.
func (r *Runner) applyCommandTrust(scope ApprovalScope, item *agents.ToolApprovalItem, sessionID string) {
	if item.ToolName != execCommandToolName || sessionID == "" || r.Deps.SandboxManager == nil {
		return
	}
	trust := r.Deps.SandboxManager.Trust().forSession(sessionID)
	switch scope {
	case ApprovalSameCommand:
		trust.allowCommand(commandHash(item.Arguments))
	case ApprovalAll:
		trust.allowAll()
	}
}
