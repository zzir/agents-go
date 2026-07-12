package bridge

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/rs/zerolog"

	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// taskSummaryLimit bounds the terminal summary written to the tasks row and
// the spawn card's display projection.
const taskSummaryLimit = 300

// TaskFinalError reports a stop attempt on an already-final task — the one
// genuinely conflict-shaped stop failure (handlers map it to 409; anything
// else is an internal error).
type TaskFinalError struct{ Status string }

func (e *TaskFinalError) Error() string { return "task already " + e.Status }

// publishTaskCancelled advances the hub record (when it still exists) and
// broadcasts run.cancelled, so every connected client flips the task's state
// and the record stops holding a concurrency-cap slot. A no-op after GC.
func (r *Runner) publishTaskCancelled(taskID string) {
	env, err := protocol.NewEnvelope(protocol.EventRunCancelled, protocol.RunCancelled{RunID: taskID})
	if err != nil {
		return
	}
	r.hub.publish(taskID, env)
	r.hub.finish(taskID, false)
}

// SpawnTask implements TaskSpawner: it creates the task's hidden child
// session and durable row, then launches the run through the ordinary run
// pipeline (same hub, same events, broadcast to every connection). The task
// id doubles as the child run id.
func (r *Runner) SpawnTask(ctx context.Context, parentSessionID, agentName, input, label string) (*TaskInfo, error) {
	cfg, err := r.resolveSpawnAgent(ctx, parentSessionID, agentName)
	if err != nil {
		return nil, err
	}

	// Snapshot the spawning run's setup so the completion notification can
	// start a parent run with the same agent/sandbox.
	var parentRunID, parentAgentConfigID, parentSandboxID string
	if rid, ok := r.hub.ActiveRunForSession(parentSessionID); ok {
		parentRunID = rid
		if info, ok := r.hub.Info(rid); ok {
			parentAgentConfigID = info.AgentConfigID
			parentSandboxID = info.SandboxID
		}
	}
	toolCallID := taskToolCallIDFrom(ctx)

	if label == "" {
		label = truncateRunes(input, 60)
	}
	taskID := store.NewID()
	child := &store.Session{ID: store.NewID(), Name: "task: " + label, AgentConfigID: cfg.ID}
	if err := r.Deps.Sessions.Create(ctx, child); err != nil {
		return nil, fmt.Errorf("spawn_task: creating task session: %w", err)
	}
	task := &store.Task{
		ID:                  taskID,
		ParentSessionID:     parentSessionID,
		ParentRunID:         parentRunID,
		ToolCallID:          toolCallID,
		Label:               label,
		AgentConfigID:       cfg.ID,
		ChildSessionID:      child.ID,
		ParentAgentConfigID: parentAgentConfigID,
		ParentSandboxID:     parentSandboxID,
		Status:              protocol.TaskWorking,
	}
	if err := r.Deps.Tasks.Create(ctx, task); err != nil {
		// Best-effort: without a tasks row the child session would surface in
		// the sidebar (the list filter keys on the row), a ghost the user can
		// open but nothing owns.
		if delErr := r.Deps.Sessions.Delete(ctx, child.ID); delErr != nil {
			zerolog.Ctx(ctx).Warn().Err(delErr).Str("session_id", child.ID).Msg("orphan task session cleanup")
		}
		return nil, fmt.Errorf("spawn_task: %w", err)
	}
	// The task shares the parent run's sandbox (and thereby its command-trust
	// scope, carried by the parent session id in the run context).
	if _, err := r.startRunWithID(taskID, child.ID, cfg.ID, parentSandboxID, input, nil); err != nil {
		// The row exists but no run will ever advance it — mark it failed so
		// task_status and the UI don't report a phantom "working" forever.
		if stErr := r.Deps.Tasks.SetStatus(ctx, taskID, protocol.TaskFailed, "failed to start: "+err.Error(), ""); stErr != nil {
			zerolog.Ctx(ctx).Warn().Err(stErr).Str("task_id", taskID).Msg("task failure status")
		}
		return nil, fmt.Errorf("spawn_task: starting task run: %w", err)
	}
	return &TaskInfo{TaskID: taskID, Label: label, Agent: cfg.Name, Status: protocol.TaskWorking}, nil
}

// maxTaskStatusWait bounds task_status's server-side wait: long enough that
// one call outlives most tasks, short enough that a stuck task returns
// control to the model.
const maxTaskStatusWait = 120 * time.Second

// TaskStatus implements TaskSpawner from the live hub when possible, falling
// back to the durable tasks row after retention GC. waitSeconds > 0 blocks —
// one cheap in-process wait — until the task reaches a final state or the
// window closes; a final status consumes the task's pending wake-up
// notification (the model just got the result in-turn; waking it again to
// repeat the news would burn a turn for nothing).
func (r *Runner) TaskStatus(ctx context.Context, taskID string, waitSeconds int) (*TaskInfo, error) {
	deadline := time.Now().Add(min(time.Duration(waitSeconds)*time.Second, maxTaskStatusWait))
	var info *TaskInfo
	for {
		task, err := r.Deps.Tasks.Get(ctx, taskID)
		if err != nil {
			return nil, fmt.Errorf("task_status: %w", err)
		}
		status := task.Status
		if hubInfo, ok := r.hub.Info(taskID); ok {
			status = TaskStatusFor(hubInfo.Status)
		}
		info = &TaskInfo{TaskID: taskID, Label: task.Label, Status: status, Summary: task.Summary, Result: task.Result}
		final := status == protocol.TaskCompleted || status == protocol.TaskFailed || status == protocol.TaskCancelled
		if final {
			r.consumeTaskNotification(task.ParentSessionID, taskID)
			return info, nil
		}
		if waitSeconds <= 0 || time.Now().After(deadline) || ctx.Err() != nil {
			return info, nil
		}
		select {
		case <-ctx.Done():
			return info, nil
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// consumeTaskNotification marks a task's result as already delivered to the
// model (via task_status) so postRun doesn't queue a duplicate wake-up, and
// drops it from the queue if it got there first.
func (r *Runner) consumeTaskNotification(parentSessionID, taskID string) {
	r.notifMu.Lock()
	defer r.notifMu.Unlock()
	if r.deliveredResults == nil {
		r.deliveredResults = map[string]bool{}
	}
	r.deliveredResults[taskID] = true
	queue := r.pendingNotifs[parentSessionID]
	for i, id := range queue {
		if id == taskID {
			r.pendingNotifs[parentSessionID] = append(queue[:i], queue[i+1:]...)
			break
		}
	}
}

// StopTask implements TaskSpawner. Only a live run can be stopped through the
// hub; a task paused on an approval is cancelled by discarding its pending
// approval (the claim that would otherwise revive it) and finalizing the row.
// Anything already final reports its actual status instead of pretending.
func (r *Runner) StopTask(taskID string, graceful bool) (*TaskInfo, error) {
	ctx := r.hub.rootCtx
	task, err := r.Deps.Tasks.Get(ctx, taskID)
	if err != nil {
		return nil, fmt.Errorf("task_stop: %w", err)
	}
	info, live := r.hub.Info(taskID)
	status := task.Status
	if live {
		status = TaskStatusFor(info.Status)
	}
	switch status {
	case protocol.TaskWorking:
		if live {
			stopped := false
			if graceful {
				stopped = r.hub.StopAfterTurn(taskID)
			}
			if !stopped {
				r.hub.Cancel(taskID)
			}
			return &TaskInfo{TaskID: taskID, Label: task.Label, Status: protocol.TaskCancelled}, nil
		}
		// No live run (e.g. after a restart): finalize the row directly.
		_ = r.Deps.Tasks.SetStatus(ctx, taskID, protocol.TaskCancelled, "stopped", "")
		r.publishTaskCancelled(taskID)
		return &TaskInfo{TaskID: taskID, Label: task.Label, Status: protocol.TaskCancelled}, nil
	case protocol.TaskInputRequired:
		// Discard the pending approval — deleting the row is the exclusive
		// claim, so a concurrent approve loses and cannot revive the task.
		if r.Deps.PendingApprovals != nil {
			if err := r.Deps.PendingApprovals.Delete(ctx, taskID); err != nil {
				if !errors.Is(err, store.ErrNotFound) {
					return nil, fmt.Errorf("task_stop: discarding pending approval: %w", err)
				}
				// The approval row is gone. Either a concurrent approve just
				// claimed it (the resume is spinning up — refuse the stop), or
				// the approval reaper expired it long ago and the task is a
				// zombie stuck at input_required — cancel that zombie below.
				if info, ok := r.hub.Info(taskID); ok && info.Status == RunRunning {
					return nil, &TaskFinalError{Status: "being resumed"}
				}
			}
		}
		if err := r.Deps.Tasks.SetStatus(ctx, taskID, protocol.TaskCancelled, "stopped while awaiting approval", ""); err != nil {
			return nil, fmt.Errorf("task_stop: %w", err)
		}
		if task.ToolCallID != "" {
			_ = r.messages.PatchToolCallDisplay(ctx, task.ParentSessionID, task.ToolCallID, map[string]any{
				"task_id": task.ID, "task_label": task.Label, "task_status": protocol.TaskCancelled,
			})
		}
		// Advance the hub record past RunInterrupted and tell every client:
		// without this the chip stays "input required" (with dead approve
		// buttons) and the record holds a task-cap slot for up to 15 minutes.
		r.publishTaskCancelled(taskID)
		return &TaskInfo{TaskID: taskID, Label: task.Label, Status: protocol.TaskCancelled}, nil
	default:
		return nil, &TaskFinalError{Status: status}
	}
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
		ParentSessionID: task.ParentSessionID,
		ParentRunID:     task.ParentRunID,
		ToolCallID:      task.ToolCallID,
		Label:           task.Label,
	}
}

// postRun runs after every run segment terminates (fresh or resumed): task
// runs get their terminal bookkeeping and parent notification; chat runs
// drain any task notifications that queued up while they were busy.
func (r *Runner) postRun(runID, sessionID string, result *RunResult) {
	ctx := r.hub.rootCtx
	log := zerolog.Ctx(ctx)
	if r.Deps.Tasks == nil {
		return
	}
	task, err := r.Deps.Tasks.ByChildSession(ctx, sessionID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			log.Warn().Err(err).Str("run_id", runID).Msg("task lookup")
		}
		r.drainTaskNotifications(sessionID)
		return
	}

	info, ok := r.hub.Info(runID)
	if !ok {
		return
	}
	status := TaskStatusFor(info.Status)
	full := strings.TrimSpace(result.FinalText)
	summary := truncateRunes(full, taskSummaryLimit)
	if err := r.Deps.Tasks.SetStatus(ctx, task.ID, status, summary, full); err != nil {
		log.Warn().Err(err).Str("task_id", task.ID).Msg("task status update")
	}
	// Patch the spawn card's display projection — the durable truth the UI
	// rebuilds the task card from after the hub record is GC'd. Retried in the
	// background: a fast-failing task can end before the parent turn persists
	// the spawn tool_call row (the SDK saves at turn boundaries).
	if task.ToolCallID != "" {
		patch := map[string]any{
			"task_id":     task.ID,
			"task_label":  task.Label,
			"task_status": status,
		}
		if summary != "" {
			patch["task_summary"] = summary
		}
		go r.patchDisplayWithRetry(ctx, task.ParentSessionID, task.ToolCallID, task.ID, patch)
	}
	// input_required is not final: the approval flow surfaces it, and the
	// resumed segment lands back here with a final status.
	if status == protocol.TaskInputRequired {
		return
	}
	// A result the model already pulled in-turn (task_status wait) owes no
	// wake-up; consume the marker instead of queueing a duplicate.
	r.notifMu.Lock()
	delivered := r.deliveredResults[task.ID]
	delete(r.deliveredResults, task.ID)
	r.notifMu.Unlock()
	if delivered {
		return
	}
	r.queueTaskNotification(task.ParentSessionID, task.ID)
	r.drainTaskNotifications(task.ParentSessionID)
}

// patchDisplayWithRetry patches the spawn card's display, retrying while the
// row hasn't been persisted yet (the parent turn saves at its boundary, which
// can be after a fast task ends). Gives up quietly after ~30s.
func (r *Runner) patchDisplayWithRetry(ctx context.Context, sessionID, callID, taskID string, patch map[string]any) {
	var err error
	for range 10 {
		if err = r.messages.PatchToolCallDisplay(ctx, sessionID, callID, patch); err == nil {
			return
		}
		if !errors.Is(err, store.ErrNotFound) {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
	}
	zerolog.Ctx(ctx).Warn().Err(err).Str("task_id", taskID).Msg("task display patch")
}

// queueTaskNotification records that a finished task still owes its parent a
// wake-up notification. In-memory only: after a restart the task card (from
// the display projection) still shows the outcome, only the auto-wake is lost.
func (r *Runner) queueTaskNotification(parentSessionID, taskID string) {
	r.notifMu.Lock()
	defer r.notifMu.Unlock()
	if r.pendingNotifs == nil {
		r.pendingNotifs = map[string][]string{}
	}
	r.pendingNotifs[parentSessionID] = append(r.pendingNotifs[parentSessionID], taskID)
}

// drainTaskNotifications wakes the parent session on queued task completions:
// it starts one notification run carrying every pending result. The parent's
// run boundary is the injection point — if the session is busy (or paused on
// an approval), the queue holds until its current run ends.
func (r *Runner) drainTaskNotifications(parentSessionID string) {
	ctx := r.hub.rootCtx
	log := zerolog.Ctx(ctx)

	r.notifMu.Lock()
	pending := r.pendingNotifs[parentSessionID]
	if len(pending) == 0 {
		r.notifMu.Unlock()
		return
	}
	if _, busy := r.hub.ActiveRunForSession(parentSessionID); busy {
		r.notifMu.Unlock()
		return
	}
	// A parent paused on approval must not be auto-woken: the human decides.
	if approvals, err := r.Deps.PendingApprovals.ListBySession(ctx, parentSessionID); err == nil && len(approvals) > 0 {
		r.notifMu.Unlock()
		return
	}
	delete(r.pendingNotifs, parentSessionID)
	r.notifMu.Unlock()

	var agentConfigID, sandboxID string
	lines := make([]string, 0, len(pending))
	for _, taskID := range pending {
		task, err := r.Deps.Tasks.Get(ctx, taskID)
		if err != nil {
			continue
		}
		if task.ParentAgentConfigID != "" {
			agentConfigID = task.ParentAgentConfigID
			sandboxID = task.ParentSandboxID
		}
		line := fmt.Sprintf("Task %q (%s) %s.", task.Label, task.ID, task.Status)
		if task.Summary != "" {
			line += " Result: " + task.Summary
			if len(task.Result) > len(task.Summary) {
				line += fmt.Sprintf(" [truncated — call task_status(%s) for the full result]", task.ID)
			}
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return
	}
	if agentConfigID == "" {
		// Fall back to the parent session's bound agent config.
		if sess, err := r.Deps.Sessions.Get(ctx, parentSessionID); err == nil {
			agentConfigID = sess.AgentConfigID
		}
	}
	if agentConfigID == "" {
		log.Warn().Str("session_id", parentSessionID).Msg("task notification dropped: no agent config for parent session")
		return
	}
	input := protocol.TaskNotificationPrefix + strings.Join(lines, "\n")
	if _, err := r.StartRun(parentSessionID, agentConfigID, sandboxID, input, nil); err != nil {
		// Lost the race with a new user run — requeue for its boundary.
		var busy ErrSessionBusy
		if errors.As(err, &busy) {
			r.notifMu.Lock()
			r.pendingNotifs[parentSessionID] = append(pending, r.pendingNotifs[parentSessionID]...)
			r.notifMu.Unlock()
			return
		}
		log.Warn().Err(err).Str("session_id", parentSessionID).Msg("task notification run failed to start")
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

// truncateRunes shortens s to at most n runes, appending an ellipsis when cut.
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	runes := []rune(s)
	return string(runes[:n]) + "…"
}
