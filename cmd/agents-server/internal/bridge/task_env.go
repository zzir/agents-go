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

// The three answers only this server can give the task manager; the state
// machine, the wake-up debt and the compare-and-set live in agents/tasks.

// taskResolver answers "what is this agent called" from the agent-config table.
type taskResolver struct{ r *Runner }

// Resolve implements tasks.AgentResolver: "default"/"self" resolve to the
// parent's own config; an unknown explicit name fails, listing what exists.
func (t taskResolver) Resolve(ctx context.Context, parentSessionID, name string) (tasks.Spec, error) {
	cfg, err := t.r.resolveSpawnAgent(ctx, parentSessionID, name)
	if err != nil {
		return tasks.Spec{}, err
	}
	// Snapshot the SPAWNING run's setup: the wake-up must use the agent the
	// parent was talking to (invariant 32).
	var parentAgentConfigID, parentProjectID string
	if rid, ok := t.r.hub.ActiveRunForSession(parentSessionID); ok {
		if info, ok := t.r.hub.Info(rid); ok {
			parentAgentConfigID, parentProjectID = info.AgentConfigID, info.ProjectID
		}
	}
	if parentAgentConfigID == "" {
		if sess, err := t.r.Deps.Sessions.Get(ctx, parentSessionID); err == nil {
			parentAgentConfigID = sess.AgentConfigID
			if parentProjectID == "" {
				parentProjectID = sess.ProjectID
			}
		}
	}
	// A parent that never ran and is bound to nothing has no agent to be woken
	// as; the task's own agent delivers then.
	if parentAgentConfigID == "" {
		parentAgentConfigID = cfg.ID
	}
	return tasks.Spec{
		DisplayName: cfg.Name,
		Inherit: store.EncodeInherit(store.Inherit{
			AgentConfigID: parentAgentConfigID,
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
		// The parent's wake-up run: the spawning run's agent and project, with
		// the lineage for the trace (invariant 32).
		if in.AgentConfigID == "" {
			return fmt.Errorf("task notification undeliverable: no agent config for session %s", req.SessionID)
		}
		_, err := t.r.StartWakeRun(req.SessionID, in.AgentConfigID, in.ProjectID, req.Input, req.ParentRunID, nil)
		return err
	}
	// The task's own run shares the parent's project, and thereby its command-
	// trust scope; the child's first run CAS-binds its hidden session with it.
	_, err := t.r.startRunWithID(req.RunID, req.SessionID, in.TaskAgentID, in.ProjectID, TextInput(req.Input), "", nil, nil)
	return err
}

// taskStopper answers "cancel this run".
type taskStopper struct{ r *Runner }

// taskStopSettleTimeout bounds the wait for a finished task run's segment to
// drain, so a stop in the "ended but not yet on the row" window reports truly.
const taskStopSettleTimeout = approvalSettleTimeout

// Stop implements tasks.Stopper. A task paused on an approval has no run to
// cancel: the approval is discarded and every client told.
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
		// No hub record (never registered, or collected): say so, and the SDK
		// records the ending itself instead of waiting on a run that never reports.
		return tasks.StopUnknownRun, nil
	}
	if isTerminalRunStatus(info.Status) {
		// Ended on its own before the stop: wait the settle window out, since a
		// closed gate means postRun wrote the row (else the SDK sees a lost outcome).
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

// onTaskUpdate is the manager's OnTaskUpdate: tell every client (task.updated)
// and append the change to the spawn call's card — invariant 21.
func (r *Runner) onTaskUpdate(ctx context.Context, t *tasks.Task) {
	r.publishTaskUpdated(ctx, t)
	if t.ToolCallID == "" {
		return
	}
	// Updates fold in append order: never record a non-terminal status over a
	// task the store already says is terminal (the CAS makes it the arbiter).
	if !isTerminalTaskStatus(string(t.Status)) && r.Deps.Tasks != nil {
		if cur, err := r.Deps.Tasks.Get(ctx, t.ID); err == nil && isTerminalTaskStatus(cur.Status) {
			return
		}
	}
	// Title/Summary are display's own fields; id and status ride in Extra.
	// An empty Summary merges as absent, so a later update cannot blank one.
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
		// Tag whose attempt the summary is (Extra merges per key), so the
		// timeline can drop a voided earlier attempt's summary.
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
// store outside the manager (the approval reaper's expiry). Tasks are an
// optional dep, as in taskMeta and the approval pause.
func (r *Runner) AnnounceTask(ctx context.Context, taskID string) {
	if r.Deps.Tasks == nil {
		return
	}
	if t, err := store.NewTaskAdapter(r.Deps.Tasks).Get(ctx, taskID); err == nil {
		r.onTaskUpdate(ctx, t)
	}
}

// publishTaskUpdated rides the task's CURRENT run stream when the hub holds it
// and is broadcast to every connection that stream misses — invariant 37.
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

// taskInfoFrom converts the SDK's task view to this server's API shape;
// MaxAttempts comes from the Runner so every response says if a retry remains.
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
