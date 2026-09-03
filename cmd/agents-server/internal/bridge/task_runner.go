package bridge

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/zzir/agents-go/agents/tasks"
	"github.com/zzir/agents-go/cmd/agents-server/internal/logging"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// TaskFinalError reports a stop attempt on an already-final task — the one
// genuinely conflict-shaped stop failure (handlers map it to 409; anything
// else is an internal error).
type TaskFinalError struct{ Status string }

func (e *TaskFinalError) Error() string { return "task already " + e.Status }

// publishTaskCancelled advances the hub record of the task's run and broadcasts
// run.cancelled; a no-op after GC or a restart.
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

// resolveSpawnAgent: an explicit config name/id wins; an empty name or the
// "default"/"self"/"current" aliases fall back to the spawning run's agent.
func (r *Runner) resolveSpawnAgent(ctx context.Context, parentSessionID, name string) (*store.AgentConfig, error) {
	// Resolved within the parent session owner's view (decisions §5.29); a
	// lookup that cannot be made refuses (spec §2.13).
	sess, err := r.Deps.Sessions.Get(ctx, parentSessionID)
	if err != nil {
		return nil, fmt.Errorf("spawn_task: resolving the parent session: %w", err)
	}
	ownerID := sess.OwnerID
	n := strings.TrimSpace(name)
	alias := n == "" || strings.EqualFold(n, "default") || strings.EqualFold(n, "self") || strings.EqualFold(n, "current")
	if n != "" {
		cfg, err := r.agentConfigByName(ctx, ownerID, n)
		if err == nil {
			return cfg, nil
		}
		if !alias {
			return nil, fmt.Errorf("spawn_task: %w", err) // explicit name: honest not-found with the available list
		}
	}
	if rid, ok := r.hub.ActiveRunForSession(parentSessionID); ok {
		if info, ok := r.hub.Info(rid); ok && info.AgentConfigID != "" {
			return r.Deps.AgentConfigs.Get(ctx, info.AgentConfigID)
		}
	}
	// No live parent run snapshot (unlikely): the session's bound agent.
	if sess.AgentConfigID != "" {
		return r.Deps.AgentConfigs.Get(ctx, sess.AgentConfigID)
	}
	return nil, fmt.Errorf("spawn_task: agent_name %q not resolvable and no current agent to default to", name)
}

// errNoSuchAgent is agentConfigByName's not-found, distinct from a store fault.
var errNoSuchAgent = errors.New("no agent")

// visibleAgentConfigs lists what ownerID may see; empty ownerID (an internal
// caller with no user) sees everything.
func (r *Runner) visibleAgentConfigs(ctx context.Context, ownerID string) ([]store.AgentConfig, error) {
	if ownerID == "" {
		return r.Deps.AgentConfigs.List(ctx)
	}
	return store.ListVisibleOf(ctx, r.Deps.AgentConfigs.CrudStore, ownerID, false)
}

// agentConfigByName resolves an agent by config name (or id) within ownerID's
// view, own over global; the not-found error lists what exists.
func (r *Runner) agentConfigByName(ctx context.Context, ownerID, name string) (*store.AgentConfig, error) {
	if cfg, err := r.Deps.AgentConfigs.Get(ctx, name); err == nil {
		if ownerID == "" || store.Visible(cfg.Scope, cfg.OwnerID, ownerID, false) {
			return cfg, nil
		}
	}
	cfgs, err := r.visibleAgentConfigs(ctx, ownerID)
	if err != nil {
		return nil, err
	}
	var match *store.AgentConfig
	names := make([]string, 0, len(cfgs))
	for i := range cfgs {
		if strings.EqualFold(cfgs[i].Name, name) {
			if store.Shadows(cfgs[i].Scope, cfgs[i].OwnerID, ownerID) {
				return &cfgs[i], nil
			}
			if match == nil {
				match = &cfgs[i]
			}
		}
		names = append(names, cfgs[i].Name)
	}
	if match != nil {
		return match, nil
	}
	slices.Sort(names)
	return nil, fmt.Errorf("%w named %q (available: %s)", errNoSuchAgent, name, strings.Join(names, ", "))
}

// taskMeta reports whether sessionID is a task's hidden child session, with its
// parent linkage (nil, nil for chat sessions). The row exists before the run starts.
func (r *Runner) taskMeta(ctx context.Context, sessionID string) (*TaskMeta, error) {
	if r.Deps.Tasks == nil {
		return nil, nil
	}
	task, err := r.Deps.Tasks.ByChildSession(ctx, sessionID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, nil
	case err != nil:
		// A check that cannot be made refuses (spec §2.13): reading a store
		// failure as "chat session" hands a TASK run the task tools — invariant 34.
		return nil, fmt.Errorf("resolving task for session %s: %w", sessionID, err)
	}
	attempt := max(task.Attempt, 1)
	return &TaskMeta{
		TaskID:          task.ID,
		Kind:            task.Kind,
		ParentSessionID: task.ParentSessionID,
		ParentRunID:     task.ParentRunID,
		ToolCallID:      task.ToolCallID,
		Label:           task.Label,
		Attempt:         attempt,
		MaxAttempts:     r.MaxTaskAttempts(),
	}, nil
}

// trustSessionID picks the session the exec_command trust gate scopes to: a
// task run inherits its parent chat session's command trust.
func trustSessionID(sessionID string, task *TaskMeta) string {
	if task != nil && task.ParentSessionID != "" {
		return task.ParentSessionID
	}
	return sessionID
}

// StopTask cancels a background task on behalf of the REST stop endpoint,
// with the same status-aware semantics as the model-facing task_stop tool.
// (The model-facing spawn/status/stop path itself is the SDK's task manager
// tools, wired in buildFullAgent — not methods here.)
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
	// A workflow at a bound is refused HERE, before the claim: after RetryClaim
	// the launcher's refusal would owe the parent a wake-up run to say so.
	if row, err := r.Deps.Tasks.Get(r.hub.rootCtx, taskID); err == nil && row.Kind == store.TaskKindWorkflow {
		if st, derr := store.DecodeWorkflowState(row.State); derr == nil {
			tokens, terr := r.executionTokens(r.hub.rootCtx, st, row.ChildSessionID)
			if terr != nil {
				return nil, fmt.Errorf("retrying task: %w", terr)
			}
			if err := st.StopIfBounded(tokens); err != nil {
				return nil, fmt.Errorf("retrying task: %w", err)
			}
		}
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

// postRun runs after every run segment terminates: drains the session's
// wake-ups, then hands the outcome to the task manager — invariant 29.
func (r *Runner) postRun(runID, sessionID string, result *RunOutcome) {
	ctx := r.hub.rootCtx
	// Any run ending is a session becoming free, which is when a debt owed to
	// it can finally be paid.
	(Waker{r}).Drain(ctx, sessionID)
	if r.tasks == nil {
		return
	}
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
// restart interrupted are failed, which owes their parents a wake-up. It runs
// synchronously, before any request — workbench invariant 32.
func (r *Runner) FailOrphanedTasks(ctx context.Context) {
	if r.tasks == nil {
		return
	}
	if err := r.tasks.FailOrphans(ctx); err != nil {
		logging.Ctx(ctx).Warn("failing tasks orphaned by the restart", "error", err)
	}
}

// DrainPendingWakeups is the second half: every session owed a turn is woken.
// It starts runs, so it runs after the handlers are wired — invariant 32.
func (r *Runner) DrainPendingWakeups(ctx context.Context) { (Waker{r}).DrainAll(ctx) }

// Shutdown drains the hub: every live run is cancelled and waited for so its
// partial turn persists before the process exits.
func (r *Runner) Shutdown(ctx context.Context) { r.hub.Shutdown(ctx) }

// SessionBusy reports whether a run is live on the session.
func (r *Runner) SessionBusy(sessionID string) bool {
	_, ok := r.hub.ActiveRunForSession(sessionID)
	return ok
}

// AbortSessionDelete undoes StopSessionTree's deleting mark after the store
// delete failed and rolled back — the session still exists and must accept
// runs again.
func (r *Runner) AbortSessionDelete(sessionID string) { r.hub.unmarkSessionDeleting(sessionID) }

// StopSessionTree cancels the session's live run and every non-terminal task it
// spawned — workflow executions are tasks, so their steps are stopped here too
// — then waits (bounded) for their goroutines to finish, postRun included, so
// the delete cascade that follows cannot race a write.
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
		// Collect the run ids BEFORE stopping: the loop below needs the list as
		// it was when they were live.
		live, err := r.Deps.Tasks.ListByParent(ctx, sessionID)
		if err != nil {
			logging.Ctx(ctx).Warn("listing tasks for session stop", "error", err, "session_id", sessionID)
		}
		for i := range live {
			if isTerminalTaskStatus(live[i].Status) {
				continue
			}
			if info, ok := r.hub.Info(live[i].RunID); ok && info.Status == RunRunning {
				waits = append(waits, live[i].RunID)
			}
		}
		// The manager cancels each and finalizes the rows nothing will advance.
		// Under the teardown deadline: it stops one at a time, and a stop can WAIT.
		stopCtx, cancelStop := context.WithDeadline(ctx, deadline)
		err = r.tasks.StopTree(stopCtx, sessionID)
		cancelStop()
		if err != nil {
			logging.Ctx(ctx).Warn("stopping session tasks", "error", err, "session_id", sessionID)
		}
	}
	for _, rid := range waits {
		r.hub.waitDone(rid, deadline)
	}
}

// sessionTeardownWait bounds how long a delete waits for run goroutines:
// unbounded lets one stuck run block forever; too short races the cascade.
const sessionTeardownWait = 5 * time.Second

// ReleaseSessionBinding evicts the cached sandbox instance behind a deleted
// session's project binding when no remaining session references the project
// (invariant 27). Called AFTER the delete cascade; best-effort — on any doubt
// the instance stays.
func (r *Runner) ReleaseSessionBinding(projectID string) {
	if projectID == "" {
		return
	}
	ctx := r.hub.rootCtx
	n, err := r.Deps.Sessions.CountProjectRefs(ctx, projectID)
	if err != nil {
		logging.Ctx(ctx).Warn("counting project binding refs", "error", err, "project_id", projectID)
		return
	}
	if n > 0 {
		return
	}
	// Eviction only: the project row still exists, so its storage and stopped
	// container stay for the next session that binds it.
	r.Deps.SandboxManager.EvictProject(projectID)
}

// ForgetSessionTrust drops a deleted session's exec_command trust grants.
// Trust is keyed by the chat session id (trustSessionID), so the root of the
// deleted tree is the key that accumulated them.
func (r *Runner) ForgetSessionTrust(sessionID string) {
	if r.Deps.SandboxManager != nil {
		r.Deps.SandboxManager.Trust().Forget(sessionID)
	}
}
