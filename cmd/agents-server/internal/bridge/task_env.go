package bridge

import (
	"context"
	"errors"
	"fmt"

	"github.com/rs/zerolog"

	"github.com/zzir/agents-go/agents/tasks"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// This file is what remains of the 669-line task runner: the three answers only
// this server can give. The state machine, the wake-up debt, the guards and the
// compare-and-set live in agents/tasks.

// taskResolver answers "what is this agent called" from the agent-config table.
type taskResolver struct{ r *Runner }

// Resolve implements tasks.AgentResolver.
//
// The aliases matter: a model asked to delegate says "default" or "self" as
// often as it names a config, and refusing those would make the tool unusable
// for the common case of "another one of me". An explicit name that does not
// exist still fails, with the available names, because that is a mistake worth
// telling the model about.
func (t taskResolver) Resolve(ctx context.Context, parentSessionID, name string) (tasks.Spec, error) {
	cfg, err := t.r.resolveSpawnAgent(ctx, parentSessionID, name)
	if err != nil {
		return tasks.Spec{}, err
	}
	// Snapshot the SPAWNING run's setup, not the task's: this payload comes
	// back when the parent is woken, and the wake-up must use the agent the
	// parent was talking to.
	var parentAgentConfigID, parentSandboxID string
	if rid, ok := t.r.hub.ActiveRunForSession(parentSessionID); ok {
		if info, ok := t.r.hub.Info(rid); ok {
			parentAgentConfigID, parentSandboxID = info.AgentConfigID, info.SandboxID
		}
	}
	if parentAgentConfigID == "" {
		if sess, err := t.r.Deps.Sessions.Get(ctx, parentSessionID); err == nil {
			parentAgentConfigID = sess.AgentConfigID
		}
	}
	return tasks.Spec{
		DisplayName: cfg.Name,
		Inherit: store.EncodeInherit(store.Inherit{
			AgentConfigID: parentAgentConfigID,
			SandboxID:     parentSandboxID,
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
		// had, so the notification is read by the agent that asked for it.
		if in.AgentConfigID == "" {
			return fmt.Errorf("task notification undeliverable: no agent config for session %s", req.SessionID)
		}
		_, err := t.r.StartRun(req.SessionID, in.AgentConfigID, in.SandboxID, req.Input, nil)
		return err
	}
	// The task's own run. It shares the parent's sandbox, and thereby its
	// command-trust scope.
	_, err := t.r.startRunWithID(req.RunID, req.SessionID, in.TaskAgentID, in.SandboxID, req.Input, nil)
	return err
}

// taskStopper answers "cancel this run".
type taskStopper struct{ r *Runner }

// Stop implements tasks.Stopper.
//
// A task paused on an approval has no run to cancel — the SDK has already
// claimed the row by the time this is called — so the work here is discarding
// the approval and telling every client, or the chip stays "input required"
// with dead buttons and holds a task-cap slot until the record is collected.
func (t taskStopper) Stop(ctx context.Context, runID string, graceful bool) error {
	info, live := t.r.hub.Info(runID)
	if live && info.Status == RunRunning {
		if graceful && t.r.hub.StopAfterTurn(runID) {
			return nil
		}
		t.r.hub.Cancel(runID)
		return nil
	}
	// Not running: either paused on an approval or already gone.
	if t.r.Deps.PendingApprovals != nil {
		if err := t.r.Deps.PendingApprovals.Delete(ctx, runID); err != nil && !errors.Is(err, store.ErrNotFound) {
			zerolog.Ctx(ctx).Warn().Err(err).Str("run_id", runID).Msg("discarding pending approval on stop")
		}
	}
	if live {
		t.r.publishTaskCancelled(runID)
	}
	return nil
}

// taskWakeGuard answers "may this parent be woken now".
//
// Three refusals, each of which was a bug: a session mid-delete would have a
// run started that outlives the cascade; a session with a live run must let
// that run's own boundary drain instead; and a session paused on a human
// decision belongs to the human. A query that fails counts as a refusal —
// waking something that turns out to be awaiting approval races the person
// looking at it.
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

// onTaskUpdate patches the spawn card's display whenever a task's state changes
// — which is long after the turn that spawned it ended.
func (r *Runner) onTaskUpdate(ctx context.Context, t *tasks.Task) {
	if t.ToolCallID == "" {
		return
	}
	patch := map[string]any{
		"task_id":     t.ID,
		"task_label":  t.Label,
		"task_status": string(t.Status),
	}
	if t.Summary != "" {
		patch["task_summary"] = t.Summary
	}
	go r.patchDisplayWithRetry(ctx, t.ParentSessionID, t.ToolCallID, t.ID, patch)
}

// taskInfoFrom converts the SDK's task view to this server's API shape.
func taskInfoFrom(i *tasks.Info) *TaskInfo {
	if i == nil {
		return nil
	}
	return &TaskInfo{
		TaskID:  i.TaskID,
		Label:   i.Label,
		Agent:   i.Agent,
		Status:  string(i.Status),
		Summary: i.Summary,
		Result:  i.Result,
	}
}

// taskStatusFromRun maps a hub run status onto the SDK's task status.
func taskStatusFromRun(s RunStatus) tasks.Status {
	return tasks.Status(TaskStatusFor(s))
}
