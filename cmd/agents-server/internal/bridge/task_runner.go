package bridge

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/zzir/agents-go/agents/tasks"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// TaskFinalError reports a stop attempt on an already-final task — the one
// genuinely conflict-shaped stop failure (handlers map it to 409; anything
// else is an internal error).
type TaskFinalError struct{ Status string }

func (e *TaskFinalError) Error() string { return "task already " + e.Status }

// publishTaskCancelled advances the hub record of the task's run (when it
// still exists) and broadcasts run.cancelled, so every connected client flips
// the task's state and the record stops holding a concurrency-cap slot. A
// no-op after GC or a restart (callers fall back to the stop API response).
func (r *Runner) publishTaskCancelled(runID string) {
	env, err := protocol.NewEnvelope(protocol.EventRunCancelled, protocol.RunCancelled{RunID: runID})
	if err != nil {
		return
	}
	r.hub.publish(runID, env)
	r.hub.finish(runID, false)
}

// isTerminalTaskStatus reports whether s is one of the three terminal task
// states (completed / failed / cancelled).
func isTerminalTaskStatus(s string) bool {
	return s == protocol.TaskCompleted || s == protocol.TaskFailed || s == protocol.TaskCancelled
}

// IsTerminalTaskStatus reports whether a task status string is terminal, for
// handlers that overlay live hub state onto stored rows and must leave
// terminal rows alone.
func IsTerminalTaskStatus(s string) bool { return isTerminalTaskStatus(s) }

// resolveSpawnAgent resolves a spawn target: an explicit config name/id wins;
// an empty name or the "default" / "self" / "current" aliases (models reach
// for these unprompted) fall back to the spawning run's own agent — a config
// actually named that way still takes precedence.
func (r *Runner) resolveSpawnAgent(ctx context.Context, parentSessionID, name string) (*store.AgentConfig, error) {
	n := strings.TrimSpace(name)
	alias := n == "" || strings.EqualFold(n, "default") || strings.EqualFold(n, "self") || strings.EqualFold(n, "current")
	if n != "" {
		cfg, err := r.agentConfigByName(ctx, n)
		if err == nil {
			return cfg, nil
		}
		if !alias {
			return nil, err // explicit name: honest not-found with the available list
		}
	}
	if rid, ok := r.hub.ActiveRunForSession(parentSessionID); ok {
		if info, ok := r.hub.Info(rid); ok && info.AgentConfigID != "" {
			return r.Deps.AgentConfigs.Get(ctx, info.AgentConfigID)
		}
	}
	// No live parent run snapshot (unlikely): the session's bound agent.
	if sess, err := r.Deps.Sessions.Get(ctx, parentSessionID); err == nil && sess.AgentConfigID != "" {
		return r.Deps.AgentConfigs.Get(ctx, sess.AgentConfigID)
	}
	return nil, fmt.Errorf("spawn_task: agent_name %q not resolvable and no current agent to default to", name)
}

// agentConfigByName resolves a spawn target by config name (or id).
func (r *Runner) agentConfigByName(ctx context.Context, name string) (*store.AgentConfig, error) {
	if cfg, err := r.Deps.AgentConfigs.Get(ctx, name); err == nil {
		return cfg, nil
	}
	cfgs, err := r.Deps.AgentConfigs.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("spawn_task: %w", err)
	}
	names := make([]string, 0, len(cfgs))
	for i := range cfgs {
		if strings.EqualFold(cfgs[i].Name, name) {
			return &cfgs[i], nil
		}
		names = append(names, cfgs[i].Name)
	}
	return nil, fmt.Errorf("spawn_task: no agent named %q (available: %s)", name, strings.Join(names, ", "))
}

// taskMeta reports whether sessionID is a task's hidden child session,
// returning its parent linkage for the run pipeline (nil, nil for chat
// sessions). The tasks row is created before the run starts, so the lookup is
// reliable on both fresh runs and approval resumes.
func (r *Runner) taskMeta(ctx context.Context, sessionID string) (*TaskMeta, error) {
	if r.Deps.Tasks == nil {
		return nil, nil
	}
	task, err := r.Deps.Tasks.ByChildSession(ctx, sessionID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, nil
	case err != nil:
		// A check that cannot be made refuses (spec §2.13). Reading a store
		// failure as "not a task session" demotes a TASK run to a chat run:
		// it is handed the task tools it must not have — so it spawns
		// grandchildren past MaxDepth — and its row is never reclaimed from
		// input_required while the run executes.
		return nil, fmt.Errorf("resolving task for session %s: %w", sessionID, err)
	}
	attempt := task.Attempt
	if attempt < 1 {
		attempt = 1
	}
	return &TaskMeta{
		TaskID:          task.ID,
		ParentSessionID: task.ParentSessionID,
		ParentRunID:     task.ParentRunID,
		ToolCallID:      task.ToolCallID,
		Label:           task.Label,
		Attempt:         attempt,
		MaxAttempts:     r.MaxTaskAttempts(),
	}, nil
}

// trustSessionID picks the session id the exec_command trust gate scopes to:
// a task run inherits its parent chat session's command trust (decision: a
// spawned task shares the parent's sandbox and its approvals).
func trustSessionID(sessionID string, task *TaskMeta) string {
	if task != nil && task.ParentSessionID != "" {
		return task.ParentSessionID
	}
	return sessionID
}

// StopTask cancels a background task on behalf of the REST stop endpoint,
// with the same status-aware semantics as the model-facing task_stop tool.
// (The model-facing spawn/status/stop path itself is the SDK's task manager
// tools, wired in buildAgent — not methods here.)
func (r *Runner) StopTask(taskID string, graceful bool) (*TaskInfo, error) {
	if r.tasks == nil {
		return nil, fmt.Errorf("task_stop: tasks are not configured")
	}
	info, err := r.tasks.Stop(r.hub.rootCtx, taskID, graceful)
	if err != nil {
		if final, ok := errors.AsType[tasks.ErrAlreadyFinal](err); ok {
			return nil, &TaskFinalError{Status: string(final.Status)}
		}
		return nil, err
	}
	return r.taskInfoFrom(info), nil
}

// RetryTask resumes a failed background task on behalf of the REST endpoint,
// with the same semantics as the model-facing task_retry tool.
//
// The hub's root context, not the request's: the run it starts outlives the
// HTTP call that asked for it, and cancelling on response would kill the
// attempt the caller was told had started.
func (r *Runner) RetryTask(taskID string) (*TaskInfo, error) {
	if r.tasks == nil {
		return nil, fmt.Errorf("task_retry: tasks are not configured")
	}
	info, err := r.tasks.Retry(r.hub.rootCtx, taskID)
	if err != nil {
		return nil, err
	}
	return r.taskInfoFrom(info), nil
}

// MaxTaskAttempts is the ceiling a task's attempt count is measured against.
// Clients get the parameter rather than a precomputed "can I retry": theirs
// then moves with the status they track live, instead of being stale from the
// moment the status changes.
func (r *Runner) MaxTaskAttempts() int {
	if r.tasks == nil {
		return 0
	}
	return r.tasks.MaxAttempts()
}

// postRun runs after every run segment terminates. It hands the outcome to the
// task manager, which advances a task's state or — for an ordinary chat session
// — drains the wake-ups that queued while it was busy.
func (r *Runner) postRun(runID, sessionID string, result *RunOutcome) {
	if r.tasks == nil {
		return
	}
	ctx := r.hub.rootCtx
	info, ok := r.hub.Info(runID)
	if !ok {
		return
	}
	out := tasks.RunOutcome{
		// The attempt that finished, so a task retried while this run was in
		// flight keeps the new attempt rather than this one's outcome.
		RunID:        runID,
		Status:       taskStatusFromRun(info.Status),
		Text:         result.FinalText,
		Err:          result.ErrMessage,
		GracefulStop: info.GracefulStop,
	}
	r.tasks.OnRunFinished(ctx, sessionID, out)
}

// FailOrphanedTasks is the first half of the restart reconciliation: tasks the
// restart interrupted are failed, which owes their parents a wake-up.
//
// It runs synchronously at startup, before the server can accept a request:
// the sweep fails every row recorded as working, so a retry that arrived first
// would have its fresh run declared dead and its result discarded.
func (r *Runner) FailOrphanedTasks(ctx context.Context) {
	if r.tasks == nil {
		return
	}
	if err := r.tasks.FailOrphans(ctx); err != nil {
		zerolog.Ctx(ctx).Warn().Err(err).Msg("failing tasks orphaned by the restart")
	}
}

// DrainPendingTaskNotifications is the second half: every parent owed a wake-up
// is drained, so the auto-wake survives a restart. It starts runs, which is why
// it is separate from the sweep above.
func (r *Runner) DrainPendingTaskNotifications(ctx context.Context) {
	if r.tasks == nil {
		return
	}
	if err := r.tasks.DrainAllPending(ctx); err != nil {
		zerolog.Ctx(ctx).Warn().Err(err).Msg("task recovery sweep")
	}
}

// Shutdown drains the hub: every live run is cancelled and waited for so its
// partial turn persists before the process exits.
func (r *Runner) Shutdown(ctx context.Context) { r.hub.Shutdown(ctx) }

// AbortSessionDelete undoes StopSessionTree's deleting mark after the store
// delete failed and rolled back — the session still exists and must accept
// runs again.
func (r *Runner) AbortSessionDelete(sessionID string) { r.hub.unmarkSessionDeleting(sessionID) }

// StopSessionTree cancels the session's live run and every non-terminal task it
// spawned, then waits (bounded) for their goroutines to finish — postRun
// included — so the delete cascade that follows cannot race a write.
//
// The teardown marker goes down FIRST: a task's drain could otherwise start a
// notification run on the session while it is being deleted, and that run would
// outlive the cascade and write into rows it is about to remove.
func (r *Runner) StopSessionTree(sessionID string) {
	ctx := r.hub.rootCtx
	r.hub.markSessionDeleting(sessionID)
	deadline := time.Now().Add(sessionTeardownWait)

	var waits []string
	if rid, ok := r.hub.ActiveRunForSession(sessionID); ok {
		r.hub.Cancel(rid)
		waits = append(waits, rid)
	}
	if r.tasks != nil {
		// Collect the run ids BEFORE stopping: once a task is finalized its
		// row still names its run, but the loop below needs the list as it was
		// when they were live.
		live, err := r.Deps.Tasks.ListByParent(ctx, sessionID)
		if err != nil {
			zerolog.Ctx(ctx).Warn().Err(err).Str("session_id", sessionID).Msg("listing tasks for session stop")
		}
		for i := range live {
			if isTerminalTaskStatus(live[i].Status) {
				continue
			}
			if info, ok := r.hub.Info(live[i].RunID); ok && info.Status == RunRunning {
				waits = append(waits, live[i].RunID)
			}
		}
		// The manager cancels each one and finalizes the rows that no goroutine
		// will ever advance. Under the teardown deadline, because it stops
		// tasks one at a time and a stop can WAIT — for a finished run's
		// outcome to reach its row — so a session's worth of them would
		// otherwise add up past any bound this delete thought it had. The
		// cascade removes these rows next in any case.
		stopCtx, cancelStop := context.WithDeadline(ctx, deadline)
		err = r.tasks.StopTree(stopCtx, sessionID)
		cancelStop()
		if err != nil {
			zerolog.Ctx(ctx).Warn().Err(err).Str("session_id", sessionID).Msg("stopping session tasks")
		}
	}
	for _, rid := range waits {
		r.hub.waitDone(rid, deadline)
	}
}

// sessionTeardownWait bounds how long a delete waits for run goroutines to
// finish. Unbounded would let one stuck run block a delete forever; too short
// and the cascade races a write.
const sessionTeardownWait = 5 * time.Second

// ReleaseSessionBinding releases the cached sandbox instance behind a deleted
// session's (sandbox, workdir) binding, but only when no remaining session
// references the pair — the instance may be a live ssh connection or a docker
// container, and sessions sharing a project must not lose it under a sibling's
// delete. Called AFTER the delete cascade, so the count excludes the deleted
// session and its task children. Best-effort: on any doubt (the count failed),
// the instance stays — an idle instance is a smaller cost than tearing one out
// from under a session still using it.
func (r *Runner) ReleaseSessionBinding(sandboxID, workDir string) {
	if sandboxID == "" {
		return
	}
	ctx := r.hub.rootCtx
	n, err := r.Deps.Sessions.CountBindingRefs(ctx, sandboxID, workDir)
	if err != nil {
		zerolog.Ctx(ctx).Warn().Err(err).Str("sandbox_id", sandboxID).Msg("counting sandbox binding refs")
		return
	}
	if n > 0 {
		return
	}
	cfg, err := r.Deps.SandboxConfigs.Get(ctx, sandboxID)
	if err != nil {
		// Config already gone: its Delete removed every cached instance.
		return
	}
	r.Deps.SandboxManager.RemoveInstance(cfg, workDir)
}
