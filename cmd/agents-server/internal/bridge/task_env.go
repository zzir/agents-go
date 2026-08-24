package bridge

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/tasks"
	"github.com/zzir/agents-go/cmd/agents-server/internal/logging"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
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
	var parentAgentConfigID, parentSandboxID, parentProjectID string
	if rid, ok := t.r.hub.ActiveRunForSession(parentSessionID); ok {
		if info, ok := t.r.hub.Info(rid); ok {
			parentAgentConfigID, parentSandboxID, parentProjectID = info.AgentConfigID, info.SandboxID, info.ProjectID
		}
	}
	if parentAgentConfigID == "" {
		if sess, err := t.r.Deps.Sessions.Get(ctx, parentSessionID); err == nil {
			parentAgentConfigID = sess.AgentConfigID
			if parentSandboxID == "" {
				parentSandboxID, parentProjectID = sess.SandboxID, sess.ProjectID
			}
		}
	}
	// A parent that has never run and is bound to nothing (a workflow started
	// over REST on a fresh session) has no agent to be woken as; the task's
	// own agent delivers then — a result nobody could deliver is worse than one
	// read by the agent that produced it.
	if parentAgentConfigID == "" {
		parentAgentConfigID = cfg.ID
	}
	return tasks.Spec{
		DisplayName: cfg.Name,
		Inherit: store.EncodeInherit(store.Inherit{
			AgentConfigID: parentAgentConfigID,
			SandboxID:     parentSandboxID,
			ProjectID:     parentProjectID,
			TaskAgentID:   cfg.ID,
		}),
	}, nil
}

// taskLauncher answers "start a run" through the hub.
type taskLauncher struct{ r *Runner }

// Launch implements tasks.Launcher.
func (t taskLauncher) Launch(ctx context.Context, req tasks.LaunchRequest) error {
	// A workflow's run is a STEP: which agent, and what to do first, come from
	// the execution's state rather than the inherit snapshot.
	if req.Kind == store.TaskKindWorkflow {
		return t.r.launchWorkflowStep(ctx, req)
	}
	in := store.DecodeInherit(req.Inherit)
	if req.Wake {
		// The parent's wake-up run: same agent and sandbox the spawning run
		// had, so the notification is read by the agent that asked for it. The
		// lineage rides along so the run's trace records which run spawned the
		// delivered task(s).
		if in.AgentConfigID == "" {
			return fmt.Errorf("task notification undeliverable: no agent config for session %s", req.SessionID)
		}
		_, err := t.r.StartWakeRun(req.SessionID, in.AgentConfigID, in.SandboxID, in.ProjectID, req.Input, req.ParentRunID, nil)
		return err
	}
	// The task's own run. It shares the parent's sandbox (and project), and
	// thereby its command-trust scope; the child's first run CAS-binds its
	// hidden session with the same pair.
	_, err := t.r.startRunWithID(req.RunID, req.SessionID, in.TaskAgentID, in.SandboxID, in.ProjectID, req.Input, "", nil, nil)
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
			logging.Ctx(ctx).Warn("discarding pending approval on stop", "error", err, "run_id", runID)
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

// onTaskUpdate is the task manager's OnTaskUpdate: it tells every connected
// client (task.updated) and records the change against the spawn call's card,
// which happens long after the turn that spawned it ended. The card update is
// appended rather than rewritten: a fast task can finish before the parent turn
// is persisted, so the update may be stored first and projection associates it
// by call id.
func (r *Runner) onTaskUpdate(ctx context.Context, t *tasks.Task) {
	r.publishTaskUpdated(ctx, t)
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
		logging.Ctx(ctx).Warn("recording task display update", "error", rerr, "task_id", t.ID)
		return
	}
	entries := store.NewEntryStoreFor(r.db, ref)
	if err := entries.AppendCallDisplayUpdate(ctx, ref, t.ToolCallID, display); err != nil {
		logging.Ctx(ctx).Warn("recording task display update", "error", err, "task_id", t.ID)
	}
}

// AnnounceTask tells the clients what a task now is, for a change made on the
// store outside the manager (the approval reaper's expiry).
func (r *Runner) AnnounceTask(ctx context.Context, taskID string) {
	if t, err := store.NewTaskAdapter(r.Deps.Tasks).Get(ctx, taskID); err == nil {
		r.onTaskUpdate(ctx, t)
	}
}

// publishTaskUpdated tells the parent session's subscribers what the task now
// is. It rides the task's CURRENT run's stream when the hub holds that run —
// replayed to a connection that attaches mid-run — and is broadcast to every
// connection that stream does not reach: all of them when there is no run (a
// task paused before its step, a transition or retry announced before its
// run registers), and the ones that joined after a run was interrupted on an
// approval, since a new connection attaches to live runs only. A startup
// sweep tells nobody either way, which is fine: nobody is connected at
// startup, and the rows are refetched on load.
func (r *Runner) publishTaskUpdated(ctx context.Context, t *tasks.Task) {
	if t == nil || t.RunID == "" || t.ParentSessionID == "" {
		return
	}
	upd := protocol.TaskUpdated{
		TaskID:          t.ID,
		ParentSessionID: t.ParentSessionID,
		ParentRunID:     t.ParentRunID,
		ToolCallID:      t.ToolCallID,
		ChildSessionID:  t.ChildSessionID,
		Kind:            t.Kind,
		Label:           t.Label,
		Status:          string(t.Status),
		Attempt:         t.AttemptNo(),
		MaxAttempts:     r.MaxTaskAttempts(),
		Summary:         t.Summary,
		State:           t.State,
		UpdatedAt:       t.UpdatedAt,
	}
	// The dismissal is the row's, not the SDK task's: read it off the row.
	if row, err := r.Deps.Tasks.Get(ctx, t.ID); err == nil {
		upd.Dismissed = row.Dismissed
	}
	// A paused task names its decision, so a client can offer it without a
	// run event to learn it from.
	if t.Status == tasks.StatusInputRequired && r.Deps.PendingApprovals != nil {
		if p, err := r.Deps.PendingApprovals.Get(ctx, t.RunID); err == nil {
			if calls := p.ParsedToolCalls(); len(calls) > 0 {
				upd.PendingCallID, upd.PendingToolName = calls[0].ToolCallID, calls[0].ToolName
			}
		}
	}
	env, err := protocol.NewEnvelope(protocol.EventTaskUpdated, upd)
	if err != nil {
		return
	}
	except := ""
	if r.hub.publish(t.RunID, env) {
		except = t.RunID
	}
	if r.OnBroadcast != nil {
		r.OnBroadcast(env, except, t.ParentSessionID)
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
		Kind:        i.Kind,
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
