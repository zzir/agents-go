package bridge

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rs/zerolog"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/tasks"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// This file holds the three answers only this server can give the task manager.
// The state machine, the wake-up debt, the guards and the compare-and-set live
// in agents/tasks.

// taskResolver answers "what is this agent called" from the agent-config table.
type taskResolver struct{ r *Runner }

// Resolve implements tasks.AgentResolver. The "default"/"self" aliases resolve
// to the parent's own config (a model delegating to "another one of me"); an
// explicit name that does not exist fails, with the available names listed.
func (t taskResolver) Resolve(ctx context.Context, parentSessionID, name string) (tasks.Spec, error) {
	cfg, err := t.r.resolveSpawnAgent(ctx, parentSessionID, name)
	if err != nil {
		return tasks.Spec{}, err
	}
	// Snapshot the SPAWNING run's setup, not the task's: this payload comes
	// back when the parent is woken, and the wake-up must use the agent the
	// parent was talking to.
	var parentAgentConfigID, parentSandboxID, parentWorkDir string
	if rid, ok := t.r.hub.ActiveRunForSession(parentSessionID); ok {
		if info, ok := t.r.hub.Info(rid); ok {
			parentAgentConfigID, parentSandboxID, parentWorkDir = info.AgentConfigID, info.SandboxID, info.WorkDir
		}
	}
	if parentAgentConfigID == "" {
		if sess, err := t.r.Deps.Sessions.Get(ctx, parentSessionID); err == nil {
			parentAgentConfigID = sess.AgentConfigID
			if parentSandboxID == "" {
				parentSandboxID, parentWorkDir = sess.SandboxID, sess.WorkDir
			}
		}
	}
	return tasks.Spec{
		DisplayName: cfg.Name,
		Inherit: store.EncodeInherit(store.Inherit{
			AgentConfigID: parentAgentConfigID,
			SandboxID:     parentSandboxID,
			WorkDir:       parentWorkDir,
			TaskAgentID:   cfg.ID,
		}),
	}, nil
}

// taskLauncher answers "start a run" through the hub.
type taskLauncher struct{ r *Runner }

// Launch implements tasks.Launcher.
func (t taskLauncher) Launch(_ context.Context, req tasks.LaunchRequest) error {
	in := store.DecodeInherit(req.Inherit)
	if req.Wake {
		// The parent's wake-up run: same agent and sandbox the spawning run
		// had, so the notification is read by the agent that asked for it. The
		// lineage rides along so the run's trace records which run spawned the
		// delivered task(s).
		if in.AgentConfigID == "" {
			return fmt.Errorf("task notification undeliverable: no agent config for session %s", req.SessionID)
		}
		_, err := t.r.StartWakeRun(req.SessionID, in.AgentConfigID, in.SandboxID, in.WorkDir, req.Input, req.ParentRunID, nil)
		return err
	}
	// The task's own run. It shares the parent's sandbox (and workdir), and
	// thereby its command-trust scope; the child's first run CAS-binds its
	// hidden session with the same pair.
	_, err := t.r.startRunWithID(req.RunID, req.SessionID, in.TaskAgentID, in.SandboxID, in.WorkDir, req.Input, "", nil)
	return err
}

// taskStopper answers "cancel this run".
type taskStopper struct{ r *Runner }

// taskStopSettleTimeout bounds the wait for a finished task run's segment to
// drain, so a stop landing in the ordinary window between "the run ended" and
// "the row says so" reports the real ending. It matches the approval settle
// wait: both wait on the same gate, for the same reason.
const taskStopSettleTimeout = approvalSettleTimeout

// Stop implements tasks.Stopper.
//
// A task paused on an approval has no run to cancel, so the work here is
// discarding the approval and telling every client — otherwise the chip stays
// "input required" with dead buttons and holds a task-cap slot.
func (t taskStopper) Stop(ctx context.Context, runID string, graceful bool) (tasks.StopOutcome, error) {
	info, live := t.r.hub.Info(runID)
	if live && info.Status == RunRunning {
		if graceful && t.r.hub.StopAfterTurn(runID) {
			return tasks.StopAfterTurn, nil
		}
		t.r.hub.Cancel(runID)
		return tasks.StopCancelled, nil
	}
	// Not running: paused on an approval, already over, or never started.
	if t.r.Deps.PendingApprovals != nil {
		if err := t.r.Deps.PendingApprovals.Delete(ctx, runID); err != nil && !errors.Is(err, store.ErrNotFound) {
			zerolog.Ctx(ctx).Warn().Err(err).Str("run_id", runID).Msg("discarding pending approval on stop")
		}
	}
	if !live {
		// The hub has no record: the run was never registered or was collected
		// long ago. Saying so lets the SDK record the ending itself instead of
		// waiting for a run that will never report.
		return tasks.StopUnknownRun, nil
	}
	if isTerminalRunStatus(info.Status) {
		// The run ended on its own before this stop arrived (the hub marks a run
		// finished before its outcome reaches the row). Nothing was cancelled, so
		// wait the window out first: the segment records its outcome (postRun)
		// then closes its done gate, so a closed gate means the row is settled.
		// The SDK treats a finished run with nothing on the row as a lost
		// outcome; answering before the gate closes would look like that. An
		// already-drained segment returns at once.
		deadline := time.Now().Add(taskStopSettleTimeout)
		if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
			// A caller stopping many tasks under one bound (a session
			// teardown) owns the budget; this wait must fit inside it.
			deadline = d
		}
		t.r.hub.waitDone(runID, deadline)
		return tasks.StopAlreadyFinished, nil
	}
	t.r.publishTaskCancelled(runID)
	return tasks.StopCancelled, nil
}

// taskWakeGuard answers "may this parent be woken now". It refuses three cases:
// a session mid-delete (a wake would outlive the cascade), a session with a live
// run (let that run's own boundary drain), and a session paused on a human
// decision (it belongs to the human). A failed query counts as a refusal.
type taskWakeGuard struct{ r *Runner }

// CanWake implements tasks.WakeGuard.
func (t taskWakeGuard) CanWake(ctx context.Context, parentSessionID string) bool {
	if t.r.hub.SessionDeleting(parentSessionID) {
		return false
	}
	if _, busy := t.r.hub.ActiveRunForSession(parentSessionID); busy {
		return false
	}
	if t.r.Deps.PendingApprovals == nil {
		return true
	}
	approvals, err := t.r.Deps.PendingApprovals.ListBySession(ctx, parentSessionID)
	if err != nil {
		zerolog.Ctx(ctx).Warn().Err(err).Str("session_id", parentSessionID).
			Msg("checking pending approvals before task wake; skipping")
		return false
	}
	return len(approvals) == 0
}

// onTaskUpdate records a task's changed state against the spawn call's card,
// which happens long after the turn that spawned it ended. It appends an update
// entry rather than rewriting the card: a fast task can finish before the parent
// turn is persisted, so the update may be stored first and projection associates
// it by call id.
func (r *Runner) onTaskUpdate(ctx context.Context, t *tasks.Task) {
	if t.ToolCallID == "" {
		return
	}
	// Updates fold in append order, so a non-terminal one landing after a
	// terminal one would roll the card back to "waiting for input" on a finished
	// task. Check the store (the CAS in Finalize makes it the arbiter) before
	// recording a status that would move the card backwards.
	if !isTerminalTaskStatus(string(t.Status)) && r.Deps.Tasks != nil {
		if cur, err := r.Deps.Tasks.Get(ctx, t.ID); err == nil && isTerminalTaskStatus(cur.Status) {
			return
		}
	}
	// Heading and one-liner are display's first-class fields; id and status stay
	// in Extra as task-renderer state. Empty Summary merges as absent (non-zero
	// fields only), so a later update cannot blank an earlier summary.
	display := agents.ItemDisplay{
		Title:   t.Label,
		Summary: t.Summary,
		Extra: map[string]any{
			"task_id":      t.ID,
			"task_status":  string(t.Status),
			"task_attempt": t.AttemptNo(),
		},
	}
	if t.Summary != "" {
		// A later update cannot blank a summary, so tag whose attempt this summary
		// is (Extra merges per key). That lets the timeline drop a summary from an
		// earlier attempt than the card is on, instead of showing a voided failure
		// as the current result.
		display.Extra["task_summary_attempt"] = t.AttemptNo()
	}
	ref, rerr := store.RefFor(ctx, r.db, t.ParentSessionID)
	if rerr != nil {
		zerolog.Ctx(ctx).Warn().Err(rerr).Str("task_id", t.ID).Msg("recording task display update")
		return
	}
	entries := store.NewEntryStoreFor(r.db, ref)
	if err := entries.AppendCallDisplayUpdate(ctx, ref, t.ToolCallID, display); err != nil {
		zerolog.Ctx(ctx).Warn().Err(err).Str("task_id", t.ID).Msg("recording task display update")
	}
}

// taskInfoFrom converts the SDK's task view to this server's API shape.
// MaxAttempts comes from the Runner (the manager's configuration), so every
// response tells a client whether a retry is still on the table.
func (r *Runner) taskInfoFrom(i *tasks.Info) *TaskInfo {
	if i == nil {
		return nil
	}
	return &TaskInfo{
		TaskID:      i.TaskID,
		Label:       i.Label,
		Agent:       i.Agent,
		Status:      string(i.Status),
		Attempt:     i.Attempt,
		MaxAttempts: r.MaxTaskAttempts(),
		Summary:     i.Summary,
		Result:      i.Result,
	}
}

// taskStatusFromRun maps a hub run status onto the SDK's task status.
func taskStatusFromRun(s RunStatus) tasks.Status {
	return tasks.Status(TaskStatusFor(s))
}
