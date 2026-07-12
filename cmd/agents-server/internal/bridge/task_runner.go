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

// SpawnTask implements TaskSpawner: it creates the task's hidden child
// session and durable row, then launches the run through the ordinary run
// pipeline (same hub, same events, broadcast to every connection).
func (r *Runner) SpawnTask(ctx context.Context, parentSessionID, agentName, input, label string) (*TaskInfo, error) {
	cfg, err := r.resolveSpawnAgent(ctx, parentSessionID, agentName)
	if err != nil {
		return nil, err
	}

	// Pre-check the cap before creating any rows, so an over-cap spawn fails
	// clean (the in-lock register check below stays as the TOCTOU backstop).
	if n := r.hub.LiveTaskCount(parentSessionID); n >= r.hub.maxTasks {
		return nil, ErrTaskLimit{Limit: r.hub.maxTasks}
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
	// Task identity and run attempt are separate ids: the task is the durable
	// entity, the run is one execution of it (a future retry would mint a new
	// run id on the same task row).
	taskID := store.NewID()
	runID := store.NewID()
	child := &store.Session{ID: store.NewID(), Name: "task: " + label, AgentConfigID: cfg.ID}
	if err := r.Deps.Sessions.Create(ctx, child); err != nil {
		return nil, fmt.Errorf("spawn_task: creating task session: %w", err)
	}
	task := &store.Task{
		ID:                  taskID,
		RunID:               runID,
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
	if _, err := r.startRunWithID(runID, child.ID, cfg.ID, parentSandboxID, input, nil); err != nil {
		// The run never started: unwind the row and the child session instead
		// of leaving a failed husk. The tool error is the model's record of
		// this attempt; a row would only pollute the task list on retries.
		if delErr := r.Deps.Tasks.DeleteByID(ctx, taskID); delErr != nil {
			zerolog.Ctx(ctx).Warn().Err(delErr).Str("task_id", taskID).Msg("unstarted task row cleanup")
		}
		if delErr := r.Deps.Sessions.Delete(ctx, child.ID); delErr != nil {
			zerolog.Ctx(ctx).Warn().Err(delErr).Str("session_id", child.ID).Msg("unstarted task session cleanup")
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
		// The durable row is the sole terminal authority: Finalize writes the
		// status, the full result, and the notification debt in one UPDATE, so
		// "row terminal" means "result fully readable". The hub only refines
		// the non-terminal display (working vs input_required) — a terminal
		// hub status whose row hasn't landed yet stays "working" for one more
		// poll tick rather than surfacing an empty result.
		status := task.Status
		if !isTerminalTaskStatus(status) {
			if hubInfo, ok := r.hub.Info(task.RunID); ok {
				if hs := TaskStatusFor(hubInfo.Status); hs == protocol.TaskWorking || hs == protocol.TaskInputRequired {
					status = hs
				}
			}
		}
		info = &TaskInfo{TaskID: taskID, Label: task.Label, Status: status, Summary: task.Summary, Result: task.Result}
		if isTerminalTaskStatus(status) {
			// The model just read the final result in-turn: consume the
			// wake-up debt so no duplicate notification run fires.
			if err := r.Deps.Tasks.ConsumeNotify(ctx, taskID); err != nil {
				zerolog.Ctx(ctx).Warn().Err(err).Str("task_id", taskID).Msg("consuming task notification")
			}
			return info, nil
		}
		if waitSeconds <= 0 || time.Now().After(deadline) || ctx.Err() != nil {
			// The wait window closing is not an error: task_status's contract is
			// to return the current snapshot when the bounded wait ends.
			return info, nil //nolint:nilerr
		}
		select {
		case <-ctx.Done():
			return info, nil
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// isTerminalTaskStatus reports whether s is one of the three terminal task
// states (completed / failed / cancelled).
func isTerminalTaskStatus(s string) bool {
	return s == protocol.TaskCompleted || s == protocol.TaskFailed || s == protocol.TaskCancelled
}

// StopTask implements TaskSpawner. A live run is stopped through the hub
// (gracefully when asked); a paused or unhosted task is finalized directly on
// the durable row via CAS — of a racing stop and approve exactly one wins.
// Anything already final reports its actual status instead of pretending.
// Cancellations never owe the parent a wake-up (the user just did it, or the
// system already annotated why): the notification debt is consumed on spot.
func (r *Runner) StopTask(taskID string, graceful bool) (*TaskInfo, error) {
	ctx := r.hub.rootCtx
	task, err := r.Deps.Tasks.Get(ctx, taskID)
	if err != nil {
		return nil, fmt.Errorf("task_stop: %w", err)
	}
	if isTerminalTaskStatus(task.Status) {
		return nil, &TaskFinalError{Status: task.Status}
	}
	info, live := r.hub.Info(task.RunID)
	status := task.Status
	if live {
		if hs := TaskStatusFor(info.Status); hs == protocol.TaskWorking || hs == protocol.TaskInputRequired {
			status = hs
		} else {
			// The run just finished; its Finalize is landing. Report the
			// imminent terminal state rather than cancelling a done task.
			return nil, &TaskFinalError{Status: TaskStatusFor(info.Status)}
		}
	}
	cancelled := &TaskInfo{TaskID: taskID, Label: task.Label, Status: protocol.TaskCancelled}
	switch status {
	case protocol.TaskWorking:
		if live && info.Status == RunRunning {
			// The run goroutine owns the row: its postRun finalizes to
			// cancelled — for graceful stops via the hub marker (set before
			// the stop signal, so a clean finish can't miss it).
			stopped := false
			if graceful {
				stopped = r.hub.StopAfterTurn(task.RunID)
			}
			if !stopped {
				r.hub.Cancel(task.RunID)
			}
			return cancelled, nil
		}
		// No live run (restart, hub GC): finalize the row directly.
		won, err := r.Deps.Tasks.Finalize(ctx, taskID, protocol.TaskCancelled, "stopped", "")
		if err != nil {
			return nil, fmt.Errorf("task_stop: %w", err)
		}
		if !won {
			cur, gerr := r.Deps.Tasks.Get(ctx, taskID)
			if gerr != nil {
				return nil, fmt.Errorf("task_stop: %w", gerr)
			}
			return nil, &TaskFinalError{Status: cur.Status}
		}
		_ = r.Deps.Tasks.ConsumeNotify(ctx, taskID)
		r.patchTaskDisplay(ctx, task, protocol.TaskCancelled, "stopped")
		r.publishTaskCancelled(task.RunID)
		return cancelled, nil
	case protocol.TaskInputRequired:
		// Finalizing the row IS the exclusive claim: a concurrent approve's
		// ReclaimWorking (input_required -> working) and this Finalize
		// (non-terminal -> cancelled) cannot both win.
		won, err := r.Deps.Tasks.Finalize(ctx, taskID, protocol.TaskCancelled, "stopped while awaiting approval", "")
		if err != nil {
			return nil, fmt.Errorf("task_stop: %w", err)
		}
		if !won {
			cur, gerr := r.Deps.Tasks.Get(ctx, taskID)
			if gerr != nil {
				return nil, fmt.Errorf("task_stop: %w", gerr)
			}
			return nil, &TaskFinalError{Status: cur.Status}
		}
		_ = r.Deps.Tasks.ConsumeNotify(ctx, taskID)
		// Discard the pending approval (best-effort: an approve may have
		// claimed it already — harmless, its ReclaimWorking lost the row CAS
		// and the resume was abandoned; and if a resume DID sneak in first,
		// cancel it so execution matches the row).
		if r.Deps.PendingApprovals != nil {
			if err := r.Deps.PendingApprovals.Delete(ctx, task.RunID); err != nil && !errors.Is(err, store.ErrNotFound) {
				zerolog.Ctx(ctx).Warn().Err(err).Str("task_id", taskID).Msg("discarding pending approval on stop")
			}
		}
		if in, ok := r.hub.Info(task.RunID); ok && in.Status == RunRunning {
			r.hub.Cancel(task.RunID)
		}
		r.patchTaskDisplay(ctx, task, protocol.TaskCancelled, "stopped while awaiting approval")
		// Advance the hub record past RunInterrupted and tell every client:
		// without this the chip stays "input required" (with dead approve
		// buttons) and the record holds a task-cap slot for up to 15 minutes.
		r.publishTaskCancelled(task.RunID)
		return cancelled, nil
	default:
		return nil, &TaskFinalError{Status: status}
	}
}

// patchTaskDisplay updates the spawn card's display projection for terminal
// transitions that happen outside the run goroutine (postRun covers the rest).
func (r *Runner) patchTaskDisplay(ctx context.Context, task *store.Task, status, summary string) {
	if task.ToolCallID == "" {
		return
	}
	patch := map[string]any{"task_id": task.ID, "task_label": task.Label, "task_status": status}
	if summary != "" {
		patch["task_summary"] = summary
	}
	if err := r.messages.PatchToolCallDisplay(ctx, task.ParentSessionID, task.ToolCallID, patch); err != nil && !errors.Is(err, store.ErrNotFound) {
		zerolog.Ctx(ctx).Warn().Err(err).Str("task_id", task.ID).Msg("task display patch")
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
		TaskID:          task.ID,
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
	// A failed run's reason travels on the result — without it the row (and
	// so the task list, the spawn card, and task_status) would only ever say
	// "failed" with no why.
	if status == protocol.TaskFailed && full == "" && result.ErrMessage != "" {
		full = result.ErrMessage
		summary = truncateRunes(full, taskSummaryLimit)
	}
	// A clean finish under the graceful-stop marker is a cancellation: the
	// user asked the task to stop after its turn, and it did. The terminal
	// state says so explicitly instead of masquerading as a completion.
	if status == protocol.TaskCompleted && info.GracefulStop {
		status = protocol.TaskCancelled
		if summary == "" {
			summary = "stopped after the current turn"
		}
	}
	// input_required is not final: the approval flow surfaces it, and the
	// resumed segment lands back here with a final status.
	if status == protocol.TaskInputRequired {
		if err := r.Deps.Tasks.MarkInputRequired(ctx, task.ID); err != nil {
			log.Warn().Err(err).Str("task_id", task.ID).Msg("task status update")
		}
		r.patchTaskDisplayRetried(ctx, task, protocol.TaskInputRequired, "")
		return
	}
	if !isTerminalTaskStatus(status) {
		return
	}
	// Finalize is a CAS: status, full result, and the wake-up debt land in
	// one UPDATE. Losing means another finalizer (a stop) already owned the
	// terminal transition — its state stands, nothing more to do here.
	won, err := r.Deps.Tasks.Finalize(ctx, task.ID, status, summary, full)
	if err != nil {
		log.Warn().Err(err).Str("task_id", task.ID).Msg("task status update")
		return
	}
	if !won {
		return
	}
	// Cancellations never wake the parent: the user (or the deleting/reaping
	// system path) initiated them and the UI already reflects it — a wake-up
	// run would only burn a turn restating it.
	if status == protocol.TaskCancelled {
		_ = r.Deps.Tasks.ConsumeNotify(ctx, task.ID)
	}
	r.patchTaskDisplayRetried(ctx, task, status, summary)
	r.drainTaskNotifications(task.ParentSessionID)
}

// patchTaskDisplayRetried patches the spawn card's display projection in the
// background — the durable truth the UI rebuilds the task card from after the
// hub record is GC'd. Retried because a fast-failing task can end before the
// parent turn persists the spawn tool_call row (the SDK saves at turn
// boundaries).
func (r *Runner) patchTaskDisplayRetried(ctx context.Context, task *store.Task, status, summary string) {
	if task.ToolCallID == "" {
		return
	}
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

// drainTaskNotifications wakes the parent session on tasks that still owe it
// a completion notification (notify_state = pending): one notification run
// carries every pending result. The parent's run boundary is the injection
// point — if the session is busy (or paused on an approval), the rows keep
// their debt until its current run ends; the startup sweep covers restarts.
// Concurrency is settled by StartRun's one-live-run-per-session guarantee:
// of two racing drains one starts the wake-up and marks the rows delivered,
// the loser leaves them untouched (still-pending rows are re-swept at the
// next boundary).
func (r *Runner) drainTaskNotifications(parentSessionID string) {
	ctx := r.hub.rootCtx
	log := zerolog.Ctx(ctx)
	if r.Deps.Tasks == nil {
		return
	}
	if _, busy := r.hub.ActiveRunForSession(parentSessionID); busy {
		return
	}
	// A parent paused on approval must not be auto-woken: the human decides.
	if approvals, err := r.Deps.PendingApprovals.ListBySession(ctx, parentSessionID); err == nil && len(approvals) > 0 {
		return
	}
	pending, err := r.Deps.Tasks.ListPendingNotify(ctx, parentSessionID)
	if err != nil {
		log.Warn().Err(err).Str("session_id", parentSessionID).Msg("listing pending task notifications")
		return
	}
	if len(pending) == 0 {
		return
	}

	var agentConfigID, sandboxID string
	lines := make([]string, 0, len(pending))
	for i := range pending {
		task := &pending[i]
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
	if agentConfigID == "" {
		// Fall back to the parent session's bound agent config.
		if sess, err := r.Deps.Sessions.Get(ctx, parentSessionID); err == nil {
			agentConfigID = sess.AgentConfigID
		}
	}
	if agentConfigID == "" {
		log.Warn().Str("session_id", parentSessionID).Msg("task notification undeliverable: no agent config for parent session")
		return
	}
	input := protocol.TaskNotificationPrefix + strings.Join(lines, "\n")
	if _, err := r.StartRun(parentSessionID, agentConfigID, sandboxID, input, nil); err != nil {
		// Lost the race with a new user run: the rows keep their pending debt
		// and the winner's boundary re-drains them.
		var busy ErrSessionBusy
		if !errors.As(err, &busy) {
			log.Warn().Err(err).Str("session_id", parentSessionID).Msg("task notification run failed to start")
		}
		return
	}
	for i := range pending {
		if err := r.Deps.Tasks.MarkNotifyDelivered(ctx, pending[i].ID); err != nil {
			log.Warn().Err(err).Str("task_id", pending[i].ID).Msg("marking task notification delivered")
		}
	}
}

// DrainPendingTaskNotifications is the startup reconciliation sweep: every
// parent session still owed a wake-up (including tasks the restart just
// marked failed) gets its notification run — the auto-wake survives restarts.
func (r *Runner) DrainPendingTaskNotifications(ctx context.Context) {
	if r.Deps.Tasks == nil {
		return
	}
	parents, err := r.Deps.Tasks.PendingNotifyParents(ctx)
	if err != nil {
		zerolog.Ctx(ctx).Warn().Err(err).Msg("startup task notification sweep")
		return
	}
	for _, sid := range parents {
		r.drainTaskNotifications(sid)
	}
}

// StopSessionTree cancels the session's live run and every non-terminal
// background task it spawned, then waits (bounded) for their goroutines to
// finish — postRun included — so the session-delete cascade that follows
// cannot race a write. Paused tasks (input_required) have no goroutine; their
// rows are finalized directly and their pending approvals fall to the cascade.
func (r *Runner) StopSessionTree(sessionID string) {
	ctx := r.hub.rootCtx
	deadline := time.Now().Add(5 * time.Second)
	var waits []string
	if rid, ok := r.hub.ActiveRunForSession(sessionID); ok {
		r.hub.Cancel(rid)
		waits = append(waits, rid)
	}
	if r.Deps.Tasks != nil {
		tasks, err := r.Deps.Tasks.ListByParent(ctx, sessionID)
		if err != nil {
			zerolog.Ctx(ctx).Warn().Err(err).Str("session_id", sessionID).Msg("listing tasks for session stop")
		}
		for i := range tasks {
			task := &tasks[i]
			if isTerminalTaskStatus(task.Status) {
				continue
			}
			if info, ok := r.hub.Info(task.RunID); ok && info.Status == RunRunning {
				// The run goroutine finalizes the row (cancelled) in postRun.
				r.hub.Cancel(task.RunID)
				waits = append(waits, task.RunID)
				continue
			}
			// No goroutine will ever advance this row — finalize it here.
			if won, err := r.Deps.Tasks.Finalize(ctx, task.ID, protocol.TaskCancelled, "parent session deleted", ""); err == nil && won {
				_ = r.Deps.Tasks.ConsumeNotify(ctx, task.ID)
			}
		}
	}
	for _, rid := range waits {
		r.hub.waitDone(rid, deadline)
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
