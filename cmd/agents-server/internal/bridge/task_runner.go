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
// returning its parent linkage for the run pipeline (nil for chat sessions).
// The tasks row is created before the run starts, so the lookup is reliable
// on both fresh runs and approval resumes.
func (r *Runner) taskMeta(ctx context.Context, sessionID string) *TaskMeta {
	if r.Deps.Tasks == nil {
		return nil
	}
	task, err := r.Deps.Tasks.ByChildSession(ctx, sessionID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			// A transient store error here would silently demote a task run to
			// a chat run (wrong routing, no cap, no parent linkage) — log it.
			zerolog.Ctx(ctx).Warn().Err(err).Str("session_id", sessionID).Msg("task meta lookup")
		}
		return nil
	}
	return &TaskMeta{
		TaskID:          task.ID,
		ParentSessionID: task.ParentSessionID,
		ParentRunID:     task.ParentRunID,
		ToolCallID:      task.ToolCallID,
		Label:           task.Label,
	}
}

// taskToolCallIDFrom pulls the spawning tool call id out of the tool context.
// SpawnTask is invoked from a FunctionTool, whose ctx carries no call id — the
// ToolContext does; the tool passes it via context because the TaskSpawner
// interface stays SDK-agnostic.
func taskToolCallIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(taskToolCallIDKey{}).(string)
	return id
}

type taskToolCallIDKey struct{}

// withTaskToolCallID tags ctx with the spawning tool call id.
func withTaskToolCallID(ctx context.Context, callID string) context.Context {
	return context.WithValue(ctx, taskToolCallIDKey{}, callID)
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

// SpawnTask implements TaskSpawner by delegating to the SDK's task manager.
//
// Everything that used to live here — creating the child session, the row, the
// rollback on a half-finished spawn, the concurrency and depth caps — is in
// agents/tasks now. What is left is the shape this server's REST API returns.
func (r *Runner) SpawnTask(ctx context.Context, parentSessionID, agentName, input, label string) (*TaskInfo, error) {
	if r.tasks == nil {
		return nil, fmt.Errorf("spawn_task: tasks are not configured")
	}
	info, err := r.tasks.Spawn(ctx, tasks.SpawnRequest{
		ParentSessionID: parentSessionID,
		AgentName:       agentName,
		Input:           input,
		Label:           label,
		// The spawning call id comes through the tool context, not the model.
		ToolCallID: taskToolCallIDFrom(ctx),
	})
	if err != nil {
		var limit tasks.ErrTaskLimit
		if errors.As(err, &limit) {
			return nil, ErrTaskLimit{Limit: limit.Limit}
		}
		return nil, err
	}
	return taskInfoFrom(info), nil
}

// TaskStatus implements TaskSpawner.
//
// The live hub still overlays the status of a running task: the row says
// "working" for the whole run, while the hub knows whether it is mid-turn or
// paused on an approval, and that difference is what the model is asking about.
func (r *Runner) TaskStatus(ctx context.Context, taskID string, waitSeconds int) (*TaskInfo, error) {
	if r.tasks == nil {
		return nil, fmt.Errorf("task_status: tasks are not configured")
	}
	info, err := r.tasks.Status(ctx, taskID, time.Duration(waitSeconds)*time.Second)
	if err != nil {
		return nil, err
	}
	out := taskInfoFrom(info)
	if !isTerminalTaskStatus(out.Status) {
		// Overlay the live run's view, but ONLY its non-terminal states. The
		// row is the truth for a terminal one: the hub finishes a run before
		// the row lands, and reporting "completed" in that window would hand
		// the model a status whose result is not readable yet.
		if task, err := r.Deps.Tasks.Get(ctx, taskID); err == nil {
			if hub, live := r.hub.Info(task.RunID); live {
				if hs := TaskStatusFor(hub.Status); !isTerminalTaskStatus(hs) {
					out.Status = hs
				}
			}
		}
	}
	return out, nil
}

// StopTask implements TaskSpawner.
func (r *Runner) StopTask(taskID string, graceful bool) (*TaskInfo, error) {
	if r.tasks == nil {
		return nil, fmt.Errorf("task_stop: tasks are not configured")
	}
	info, err := r.tasks.Stop(r.hub.rootCtx, taskID, graceful)
	if err != nil {
		var final tasks.ErrAlreadyFinal
		if errors.As(err, &final) {
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
