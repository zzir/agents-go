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

// persistInterruption serializes an interrupted run's SDK state and its
// pending tool calls to the store, so the approval survives a restart and can
// be resumed from any connection. Best-effort: a persistence failure is
// logged by the caller, and the in-memory hub still holds the live run.
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
	return r.Deps.PendingApprovals.Save(context.Background(), &store.PendingApproval{
		RunID:         result.RunID,
		SessionID:     result.SessionID,
		AgentConfigID: result.AgentConfigID,
		SandboxID:     result.SandboxID,
		State:         string(stateJSON),
		ToolCalls:     callsJSON,
		// The user-authored text of the paused turn's new input, so the UI can
		// rebuild the user bubble on reload — the SDK only writes the turn to
		// the session once it completes.
		UserInput: session.UserText(result.SDKState.UserInput),
	})
}

// buildAgentRegistry builds the agent from its config and returns a name→agent
// registry covering it and all reachable handoff targets, as required by
// agents.RunStateFromJSON. It must build with the run's sandboxID: the
// restored state's CurrentAgent is resolved FROM this registry and is the very
// agent the SDK re-runs, so omitting the sandbox here strips its
// sandbox-backed tools (exec_command, read_file, …) and the approved call
// fails with "tool not found on agent".
func (r *Runner) buildAgentRegistry(ctx context.Context, agentConfigID, sandboxID string, taskRun bool) (map[string]*agents.Agent, *BuildResult, error) {
	built, err := buildFullAgent(ctx, r.Deps, agentConfigID, sandboxID, taskRun)
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

// armPlanUnlock makes persisting the plan_unlocked annotation the
// PRECONDITION of the phase's first unlock: the hook's error fails the
// unlock, so the run is never executing ahead of its durable record — a
// failed write surfaces as a submit_plan tool error and the review repeats.
func armPlanUnlock(phase *middleware.PlanPhase, sa *store.EntryStore) {
	if phase == nil {
		return
	}
	phase.OnUnlock(func() error {
		ctx, cancel := context.WithTimeout(context.Background(), planUnlockPersistTimeout)
		defer cancel()
		entry := session.NewAnnotationEntry(
			agents.ItemDisplay{Kind: store.PlanUnlockedKind},
			agents.Source{Type: agents.SourceTool, ID: middleware.PlanToolName},
		)
		if err := sa.Append(ctx, entry); err != nil {
			return fmt.Errorf("persisting the plan-unlock record: %w", err)
		}
		return nil
	})
}

// restorePlanPhase puts a rebuilt plan-mode run back into the phase its
// durable record says it is in, and arms marker persistence for the unlock
// this resume may perform. A marker READ failure is an error, not a warning:
// nothing has been claimed yet, so failing here leaves the pending approval
// intact for a retry — silently resuming in the planning phase would strip a
// mid-execution run of its write tools.
//
// The annotation stays the truth here even though agents.RunState.Extra could
// carry the same bit: the marker is written the moment the unlock EXECUTES
// (OnUnlock persists as a precondition), so it survives a crash with no pause
// — a window Extra, written only when a pause serializes state, cannot cover.
func (r *Runner) restorePlanPhase(ctx context.Context, phase *middleware.PlanPhase, sessionID, runID string) error {
	if phase == nil {
		return nil
	}
	ref, err := store.RefFor(ctx, r.db, sessionID)
	if err != nil {
		return fmt.Errorf("resolving session for plan phase: %w", err)
	}
	sa := store.NewEntryStoreFor(r.db, ref)
	sa.SetRunID(runID)
	unlocked, err := sa.RunHasAnnotation(ctx, runID, store.PlanUnlockedKind)
	if err != nil {
		return fmt.Errorf("reading plan-unlock marker: %w", err)
	}
	if unlocked {
		// No hook is armed yet, so this cannot fail — arming AFTER is also
		// what keeps a replayed unlock from writing a second marker.
		_ = phase.Unlock()
	}
	armPlanUnlock(phase, sa)
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
	registry, rebuilt, err := r.buildAgentRegistry(ctx, pending.AgentConfigID, pending.SandboxID, taskMeta != nil)
	if err != nil {
		return "", pending.SessionID, fmt.Errorf("rebuilding agent: %w", err)
	}
	state, err := agents.RunStateFromJSON([]byte(pending.State), registry)
	if err != nil {
		return "", pending.SessionID, fmt.Errorf("restoring run state: %w", err)
	}
	// A plan-mode rebuild starts in the planning phase, but this run may have
	// moved past it: without the unlock, a pause AFTER the plan phase ended
	// (an exec_command approval, say) would resume into a run whose write
	// tools had vanished again. The durable truth is the plan_unlocked
	// annotation, written the moment the approved submit_plan EXECUTED. A
	// failed read aborts the resolve — the pending approval is still
	// unclaimed at this point, so the decision simply retries.
	if err := r.restorePlanPhase(ctx, rebuilt.PlanPhase, pending.SessionID, pending.RunID); err != nil {
		return "", pending.SessionID, err
	}

	item := findApprovalItem(state, toolCallID)
	if item == nil {
		return "", pending.SessionID, fmt.Errorf("tool call %s not found in run state", toolCallID)
	}
	if approve {
		state.Approve(item, false)
		// A task's exec_command gate reads the PARENT session's trust store
		// (trustSessionID); record the grant under the same key or it would
		// never be consulted again.
		r.applyCommandTrust(scope, item, trustSessionID(pending.SessionID, taskMeta))
	} else {
		state.Reject(item, false, reason)
	}

	// Wait for the paused segment's teardown to complete before claiming. The
	// run goroutine's postRun marks a task input_required (working ->
	// input_required) and only THEN closes the segment's done gate, so waiting
	// on it guarantees the task row is already in the state ReclaimWorking
	// expects. This closes the window: a fast approve that raced ahead of
	// postRun used to delete the pending row, then fail ReclaimWorking (task
	// still "working"), and — with the row gone — strand the approval forever.
	r.hub.waitDone(pending.RunID, time.Now().Add(approvalSettleTimeout))

	// Deleting the record is the exclusive claim on this approval vs. a
	// concurrent approve: Delete reports ErrNotFound when the row is already
	// gone, so of two racing decisions exactly one proceeds. It must precede the
	// resume — the continuation may itself interrupt and persist a fresh record.
	if err := r.Deps.PendingApprovals.Delete(mctx, pending.RunID); err != nil {
		return "", pending.SessionID, fmt.Errorf("claiming pending approval: %w", err)
	}

	// For a task's approval the row CAS (input_required -> working) is the
	// second claim, mutually exclusive with a concurrent stop's Finalize.
	if taskMeta != nil && taskMeta.TaskID != "" {
		won, cerr := r.Deps.Tasks.ReclaimWorking(mctx, taskMeta.TaskID)
		if cerr != nil {
			// A store error is not a definitive loss — put the row back so the
			// decision survives to be retried instead of vanishing with the run.
			r.restorePendingApproval(mctx, pending)
			return "", pending.SessionID, fmt.Errorf("reclaiming task %s: %w", taskMeta.TaskID, cerr)
		}
		if !won {
			// The task is no longer reclaimable. If it went terminal a stop/reap
			// won the race and the decision is genuinely void (the discarded row
			// is correct — nothing may revive a cancelled task). If it is somehow
			// still non-terminal (the settle wait should have prevented this),
			// restore the row so the approval is not lost — defense for it.
			cur, gerr := r.Deps.Tasks.Get(mctx, taskMeta.TaskID)
			if gerr == nil && !isTerminalTaskStatus(cur.Status) {
				r.restorePendingApproval(mctx, pending)
				return "", pending.SessionID, &ApprovalNotReadyError{RunID: pending.RunID}
			}
			return "", pending.SessionID, &ApprovalVoidError{TaskID: taskMeta.TaskID}
		}
	}

	// The continuation reopens the SAME run id, so the whole turn — both the
	// interrupted and resumed halves — shares one event stream and trace group.
	runID, err = r.ResumeRun(pending.RunID, state, pending.SessionID, pending.AgentConfigID, pending.SandboxID, onDone)
	if err != nil {
		// Give the approval back (e.g. the session has a live run right now) so
		// the decision can be retried once the session frees up — losing the row
		// here would strand the paused run forever. The task row goes back to
		// input_required with it.
		r.restorePendingApproval(mctx, pending)
		if taskMeta != nil && taskMeta.TaskID != "" {
			if merr := r.Deps.Tasks.MarkInputRequired(mctx, taskMeta.TaskID); merr != nil {
				zerolog.Ctx(ctx).Warn().Err(merr).Str("task_id", taskMeta.TaskID).Msg("restoring task input_required after failed resume")
			}
		}
		return "", pending.SessionID, err
	}

	// A stop may have finalized the task cancelled in the narrow window between
	// our ReclaimWorking and the resumed segment registering as live — its own
	// cancel would then have found no live run to stop. Re-check: if the task is
	// now terminal, the run we just started is a zombie (executing under a
	// cancelled task), so cancel it to keep execution consistent with the row.
	if taskMeta != nil && taskMeta.TaskID != "" {
		if cur, gerr := r.Deps.Tasks.Get(mctx, taskMeta.TaskID); gerr == nil && isTerminalTaskStatus(cur.Status) {
			r.hub.Cancel(runID)
		}
	}
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
