package bridge

import (
	"context"
	"strings"

	"github.com/zzir/agents-go/agents/tasks"
	"github.com/zzir/agents-go/cmd/agents-server/internal/logging"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// WakeKindTask is the one wake kind — a task, of any kind, owes the turn.
// Aliased from the store, where the wake-up model lives (a task's debt is
// written there, atomically with the task's terminal state).
const WakeKindTask = store.WakeKindTask

// Waker turns finished background work into a turn on the session that asked
// for it. The debt is a ROW (store.Wakeup), drained at the moments a session
// becomes able to take one — the end of any run on it, and startup — and one
// drain pays every same-inherit debt the session has. workbench invariant 32.
type Waker struct{ r *Runner }

// Owe records that sessionID is owed a turn. The caller fills Kind, SourceID,
// Payload, Inherit and ParentRunID; everything else is the row's own.
func (w Waker) Owe(ctx context.Context, wk *store.Wakeup) error {
	if w.r.Deps.Wakeups == nil || wk.SessionID == "" {
		return nil
	}
	return w.r.Deps.Wakeups.Owe(ctx, wk)
}

// Cancel drops what a source still owes for one attempt (a task's run id):
// its result already reached the session another way, or the work was
// cancelled and a turn restating that would only repeat what the user just did.
func (w Waker) Cancel(ctx context.Context, kind, sourceID, attempt string) {
	if w.r.Deps.Wakeups == nil {
		return
	}
	if err := w.r.Deps.Wakeups.CancelFor(ctx, kind, sourceID, attempt); err != nil {
		logging.Ctx(ctx).Warn("cancelling a wake-up debt", "error", err, "kind", kind, "source_id", sourceID)
	}
}

// Drain wakes the session with everything it is owed, if it can be woken now.
// A refusal is not a failure: the debts stay pending and the next boundary
// tries again.
func (w Waker) Drain(ctx context.Context, sessionID string) {
	if w.r.Deps.Wakeups == nil || sessionID == "" {
		return
	}
	if !w.canWake(ctx, sessionID) {
		return
	}
	log := logging.Ctx(ctx)
	pending, err := w.r.Deps.Wakeups.Pending(ctx, sessionID)
	if err != nil {
		log.Warn("listing wake-up debts", "error", err, "session_id", sessionID)
		return
	}
	// A debt with no agent config was born undeliverable: Inherit is frozen at
	// write, so no later boundary will ever do better. Cancel it now instead of
	// letting it sit pending and re-warn at every boundary forever.
	deliverable := make([]store.Wakeup, 0, len(pending))
	for i := range pending {
		if store.DecodeInherit([]byte(pending[i].Inherit)).AgentConfigID == "" {
			log.Warn("wake-up carries no agent config; cancelled as undeliverable", "wakeup_id", pending[i].ID, "session_id", sessionID)
			if _, err := w.r.Deps.Wakeups.Settle(ctx, pending[i].ID, pending[i].Attempt, store.WakeCancelled); err != nil {
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

	if _, err := w.r.StartWakeRun(sessionID, inherit.AgentConfigID, inherit.SandboxID, inherit.ProjectID,
		strings.Join(payloads, "\n\n"), parentRunID, nil); err != nil {
		// Lost a race with a run that started between the guard and here. The
		// debts stay pending and that run's own boundary re-drains them.
		log.Debug("wake-up run did not start", "error", err, "session_id", sessionID)
		return
	}
	// Settled only AFTER the launch, and bound to the attempt this batch read:
	// the launch is long enough for a retry to reopen one of these, and marking
	// THAT delivered would bury a result nobody has heard. Only THIS group is
	// settled; a different-inherit debt stays pending for its own turn.
	for i := range batch {
		if _, err := w.r.Deps.Wakeups.Settle(ctx, batch[i].ID, batch[i].Attempt, store.WakeDelivered); err != nil {
			log.Warn("marking a wake-up delivered", "error", err, "wakeup_id", batch[i].ID)
		}
	}
}

// oldestInheritGroup selects the debts one wake turn may deliver together: the
// oldest, plus every later debt with the SAME inherit. Debts with a different
// inherit stay out — a turn runs as one agent, and the next boundary delivers
// them as their own turn. pending must be non-empty and every entry
// deliverable (Drain filters the config-less ones out first).
func oldestInheritGroup(pending []store.Wakeup) (batch []store.Wakeup, inherit store.Inherit, parentRunID string) {
	anchor := pending[0]
	for i := range pending {
		if pending[i].Inherit == anchor.Inherit {
			batch = append(batch, pending[i])
		}
	}
	return batch, store.DecodeInherit([]byte(anchor.Inherit)), anchor.ParentRunID
}

// canWake answers "may this session be woken now". It refuses three cases: a
// session mid-delete (a wake would outlive the cascade), a session with a live
// run (let that run's own boundary drain), and a session paused on a human
// decision (it belongs to the human). A failed query counts as a refusal —
// "cannot prove it is safe" is not permission.
func (w Waker) canWake(ctx context.Context, sessionID string) bool {
	if w.r.hub.SessionDeleting(sessionID) {
		return false
	}
	if _, busy := w.r.hub.ActiveRunForSession(sessionID); busy {
		return false
	}
	if w.r.Deps.PendingApprovals == nil {
		return true
	}
	approvals, err := w.r.Deps.PendingApprovals.ListBySession(ctx, sessionID)
	if err != nil {
		logging.Ctx(ctx).Warn("checking pending approvals before a wake-up; skipping", "error", err, "session_id", sessionID)
		return false
	}
	return len(approvals) == 0
}

// DrainAll pays every session owed something — the restart sweep. Runs after
// the reconciliation that decides which work died with the process, so the
// debts it finds are complete.
func (w Waker) DrainAll(ctx context.Context) {
	if w.r.Deps.Wakeups == nil {
		return
	}
	sessions, err := w.r.Deps.Wakeups.PendingSessions(ctx)
	if err != nil {
		logging.Ctx(ctx).Warn("listing sessions owed a wake-up", "error", err)
		return
	}
	for _, id := range sessions {
		w.Drain(ctx, id)
	}
}

// taskFinished is the tasks manager's OnFinished: a task reached a terminal
// state and its parent has not heard. The DEBT was already written — atomically
// with the task's terminal state, inside the store's Finalize/FailOrphans — so
// this only tries to PAY it now; a crash before this line still leaves the row
// behind, and the next drain (a run boundary, or startup) settles it.
func (r *Runner) taskFinished(ctx context.Context, t *tasks.Task) {
	if t == nil || t.ParentSessionID == "" {
		return
	}
	(Waker{r}).Drain(ctx, t.ParentSessionID)
}

// taskResultDelivered is OnResultDelivered: the model pulled the result in-turn,
// so the debt is moot.
func (r *Runner) taskResultDelivered(ctx context.Context, t *tasks.Task) {
	if t != nil {
		(Waker{r}).Cancel(ctx, WakeKindTask, t.ID, t.RunID)
	}
}
