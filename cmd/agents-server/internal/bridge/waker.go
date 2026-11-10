package bridge

import (
	"context"
	"strings"

	"github.com/zzir/agents-go/agents/tasks"
	"github.com/zzir/agents-go/cmd/agents-server/internal/logging"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// The waker turns finished background work into a turn on the session that
// asked for it: a store.Wakeup row, drained at the end of any run on the
// session and at startup — invariant 32. This file is the one place it lives.

// cancelWakeup drops what a task still owes for one attempt (its run id): the
// result reached the session another way, or the work was cancelled.
func (r *Runner) cancelWakeup(ctx context.Context, taskID, attempt string) {
	if err := r.Deps.Wakeups.CancelFor(ctx, store.WakeKindTask, taskID, attempt); err != nil {
		logging.Ctx(ctx).Warn("cancelling a wake-up debt", "error", err, "task_id", taskID)
	}
}

// drainWakeups wakes the session with everything it is owed, if it can be
// woken now; a refusal leaves the debts pending for the next boundary.
func (r *Runner) drainWakeups(ctx context.Context, sessionID string) {
	if sessionID == "" || !r.canWake(ctx, sessionID) {
		return
	}
	log := logging.Ctx(ctx)
	pending, err := r.Deps.Wakeups.Pending(ctx, sessionID)
	if err != nil {
		log.Warn("listing wake-up debts", "error", err, "session_id", sessionID)
		return
	}
	// A debt with no agent config is undeliverable for good (Inherit is
	// frozen): cancelled — invariant 32.
	deliverable := make([]store.Wakeup, 0, len(pending))
	for i := range pending {
		if store.DecodeInherit([]byte(pending[i].Inherit)).AgentConfigID == "" {
			log.Warn("wake-up carries no agent config; cancelled as undeliverable", "wakeup_id", pending[i].ID, "session_id", sessionID)
			if _, err := r.Deps.Wakeups.Settle(ctx, pending[i].ID, pending[i].Attempt, store.WakeCancelled); err != nil {
				log.Warn("cancelling an undeliverable wake-up", "error", err, "wakeup_id", pending[i].ID)
			}
			continue
		}
		deliverable = append(deliverable, pending[i])
	}
	if len(deliverable) == 0 {
		return
	}

	batch, inherit, parentRunID := oldestInheritGroup(deliverable)
	payloads := make([]string, 0, len(batch))
	for i := range batch {
		payloads = append(payloads, batch[i].Payload)
	}

	if _, err := r.startWakeRun(sessionID, inherit.AgentConfigID, inherit.WorkspaceID,
		strings.Join(payloads, "\n\n"), parentRunID, r.wakeWithheld(ctx, batch)); err != nil {
		// Lost a race with a run that started meanwhile; its own boundary re-drains.
		log.Debug("wake-up run did not start", "error", err, "session_id", sessionID)
		return
	}
	// Settled only AFTER the launch and bound to the attempt read; only THIS
	// group — a different-inherit debt stays pending for its own turn.
	for i := range batch {
		if _, err := r.Deps.Wakeups.Settle(ctx, batch[i].ID, batch[i].Attempt, store.WakeDelivered); err != nil {
			log.Warn("marking a wake-up delivered", "error", err, "wakeup_id", batch[i].ID)
		}
	}
}

// oldestInheritGroup selects the debts one turn may deliver: the oldest plus
// every later one with the SAME inherit. pending is non-empty and deliverable.
func oldestInheritGroup(pending []store.Wakeup) (batch []store.Wakeup, inherit store.Inherit, parentRunID string) {
	anchor := pending[0]
	for i := range pending {
		if pending[i].Inherit == anchor.Inherit {
			batch = append(batch, pending[i])
		}
	}
	return batch, store.DecodeInherit([]byte(anchor.Inherit)), anchor.ParentRunID
}

// canWake refuses a session mid-delete, with a live run, or paused on a human
// decision; a failed query is a refusal too.
func (r *Runner) canWake(ctx context.Context, sessionID string) bool {
	if r.hub.SessionDeleting(sessionID) {
		return false
	}
	if _, busy := r.hub.ActiveRunForSession(sessionID); busy {
		return false
	}
	paused, err := r.pausedOnApproval(ctx, sessionID)
	if err != nil {
		logging.Ctx(ctx).Warn("checking pending approvals before a wake-up; skipping", "error", err, "session_id", sessionID)
		return false
	}
	return !paused
}

// DrainPendingWakeups pays every session owed something: the second half of
// the restart reconciliation, after FailOrphanedTasks wrote every debt. It
// starts runs, so it runs after the handlers are wired — invariant 32.
func (r *Runner) DrainPendingWakeups(ctx context.Context) {
	sessions, err := r.Deps.Wakeups.PendingSessions(ctx)
	if err != nil {
		logging.Ctx(ctx).Warn("listing sessions owed a wake-up", "error", err)
		return
	}
	for _, id := range sessions {
		r.drainWakeups(ctx, id)
	}
}

// taskFinished is the manager's OnFinished: the debt is already written with
// the terminal state (invariant 32), so this only tries to PAY it now.
func (r *Runner) taskFinished(ctx context.Context, t *tasks.Task) {
	if t == nil || t.ParentSessionID == "" {
		return
	}
	r.drainWakeups(ctx, t.ParentSessionID)
}

// taskResultDelivered is OnResultDelivered: the model pulled the result in-turn,
// so the debt is moot.
func (r *Runner) taskResultDelivered(ctx context.Context, t *tasks.Task) {
	if t != nil {
		r.cancelWakeup(ctx, t.ID, t.RunID)
	}
}
