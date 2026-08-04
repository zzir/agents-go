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

// taskSummaryLimit bounds the terminal summary written to the tasks row and

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
	return &TaskMeta{
		TaskID:          task.ID,
		ParentSessionID: task.ParentSessionID,
		ParentRunID:     task.ParentRunID,
		ToolCallID:      task.ToolCallID,
		Label:           task.Label,
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
	return taskInfoFrom(info), nil
}

// postRun runs after every run segment terminates. It hands the outcome to the
// task manager, which advances a task's state or — for an ordinary chat session
// — drains the wake-ups that queued while it was busy.
func (r *Runner) postRun(runID, sessionID string, result *RunResult) {
	if r.tasks == nil {
		return
	}
	ctx := r.hub.rootCtx
	info, ok := r.hub.Info(runID)
	if !ok {
		return
	}
	out := tasks.RunOutcome{
		Status:       taskStatusFromRun(info.Status),
		Text:         result.FinalText,
		Err:          result.ErrMessage,
		GracefulStop: info.GracefulStop,
	}
	r.tasks.OnRunFinished(ctx, sessionID, out)
}

// DrainPendingTaskNotifications is the startup reconciliation sweep: tasks the
// restart interrupted are failed (which owes their parents a wake-up) and every
// owed parent is drained, so the auto-wake survives a restart.
func (r *Runner) DrainPendingTaskNotifications(ctx context.Context) {
	if r.tasks == nil {
		return
	}
	if err := r.tasks.Recover(ctx); err != nil {
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
		// will ever advance.
		if err := r.tasks.StopTree(ctx, sessionID); err != nil {
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
