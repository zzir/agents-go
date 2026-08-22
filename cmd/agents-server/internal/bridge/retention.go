package bridge

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/zzir/agents-go/cmd/agents-server/internal/logging"
	"github.com/zzir/agents-go/cmd/agents-server/internal/settings"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// RunApprovalReaper expires pending tool approvals that have gone unanswered
// past the TTL. On expiry it drops the record and writes a session annotation
// so the timeout is visible instead of silently vanishing. It runs at startup
// and hourly until ctx ends — run it in a goroutine.
func RunApprovalReaper(ctx context.Context, cfg *settings.Reader, approvals *store.PendingApprovalStore, entries *store.EntryStore, tasks *store.TaskStore, announce func(ctx context.Context, taskID string)) {
	log := logging.Ctx(ctx)
	reap := func() {
		ttl := cfg.Int(ctx, settings.KeyApprovalTTLMinutes)
		if ttl <= 0 {
			return // expiry disabled
		}
		cutoff := time.Now().UTC().Add(-time.Duration(ttl) * time.Minute)
		expired, err := approvals.ListOlderThan(ctx, cutoff)
		if err != nil {
			log.Error("approval reaper failed", "error", err)
			return
		}
		for _, p := range expired {
			// Each row is claimed on its own — deleted in the SAME transaction
			// as the task it ends, when it belongs to one: an approval that
			// expires ends its task, and neither half can land without the
			// other, whatever fails or crashes in between. A decision racing
			// the reaper takes the row first or not at all; a stale row (its
			// task moved to another attempt, or ended) is removed and moves
			// nothing.
			var task *store.Task
			claimed := false
			if tasks != nil {
				var terr error
				task, terr = tasks.ByChildSession(ctx, p.SessionID)
				if terr != nil && !errors.Is(terr, store.ErrNotFound) {
					log.Warn("expiring an approval: its task could not be read; kept for the next round", "error", terr, "run_id", p.RunID)
					continue
				}
			}
			if task != nil {
				// Against p.RunID — the attempt this expired approval belongs
				// to — NEVER the row's current run id: after a crash +
				// FailOrphans + retry, the row names the retry's run, and
				// ending against it would cancel a healthy new attempt because
				// an approval from a previous life expired.
				var ended bool
				claimed, ended, err = tasks.ClaimApprovalCancelled(ctx, task.ID, p.RunID, "approval expired after "+strconv.Itoa(ttl)+" minutes")
				if err != nil {
					log.Warn("expiring an approval; kept for the next round", "error", err, "run_id", p.RunID)
					continue
				}
				if ended && announce != nil {
					// Cancellations owe no wake-up (dropped in the same write);
					// the parent learns of it through the task's own state.
					announce(ctx, task.ID)
				}
			} else if derr := approvals.Delete(ctx, p.RunID); derr == nil {
				claimed = true
			} else if !errors.Is(derr, store.ErrNotFound) {
				log.Warn("expiring an approval; kept for the next round", "error", derr, "run_id", p.RunID)
				continue
			}
			if !claimed {
				continue // a decision took it first
			}
			// The banner goes to the session the approval was filed on — a
			// task's or a step's hidden child session, whose transcript is
			// where the pause happened.
			if ref, rerr := entries.RefFor(ctx, p.SessionID); rerr != nil {
				log.Warn("cannot record an approval-timeout banner", "error", rerr, "session_id", p.SessionID)
			} else {
				banner := "Tool approval timed out after " + strconv.Itoa(ttl) + " minutes; the run was terminated."
				if p.Kind == store.ApprovalKindStep {
					banner = "Step approval timed out after " + strconv.Itoa(ttl) + " minutes; the workflow was cancelled."
				}
				_ = entries.AppendAnnotation(ctx, ref, p.RunID, banner)
			}
			log.Info("expired pending approval", "run_id", p.RunID, "session_id", p.SessionID)
		}
	}

	reap()
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			reap()
		}
	}
}

// RunTraceRetention prunes old trace events at startup and then once a day,
// controlled by the trace_retention_days setting (a positive integer number
// of days; unset, zero, or invalid disables pruning). It blocks until ctx
// ends — run it in a goroutine.
func RunTraceRetention(ctx context.Context, cfg *settings.Reader, traces *store.TraceStore) {
	log := logging.Ctx(ctx)
	prune := func() {
		days := cfg.Int(ctx, settings.KeyTraceRetentionDays)
		if days <= 0 {
			return // unset or zero — retention disabled
		}
		cutoff := time.Now().UTC().AddDate(0, 0, -days)
		n, err := traces.DeleteOlderThan(ctx, cutoff)
		if err != nil {
			log.Error("trace retention prune failed", "error", err)
			return
		}
		if n > 0 {
			log.Info("pruned old trace events", "removed", n, "retention_days", days)
		}
	}

	prune()
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			prune()
		}
	}
}

// RunAuthTokenCleanup deletes expired session tokens and PATs at startup and
// then hourly — the maintenance half of the lazy delete Authenticate does in
// passing. It blocks until ctx ends — run it in a goroutine.
func RunAuthTokenCleanup(ctx context.Context, tokens *store.AuthTokenStore) {
	log := logging.Ctx(ctx)
	sweep := func() {
		n, err := tokens.DeleteExpired(ctx)
		if err != nil {
			log.Error("auth token cleanup failed", "error", err)
			return
		}
		if n > 0 {
			log.Info("removed expired auth tokens", "removed", n)
		}
	}

	sweep()
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweep()
		}
	}
}

// wakeupRetention is how long a settled wake-up row stays readable after the
// fact — long enough to inspect a recent task's delivery, not forever.
const wakeupRetention = 7 * 24 * time.Hour

// RunWakeupCleanup prunes settled wake-ups older than wakeupRetention at
// startup and then hourly. It blocks until ctx ends — run it in a goroutine.
func RunWakeupCleanup(ctx context.Context, wakeups *store.WakeupStore) {
	log := logging.Ctx(ctx)
	sweep := func() {
		n, err := wakeups.DeleteSettledBefore(ctx, time.Now().UTC().Add(-wakeupRetention))
		if err != nil {
			log.Error("wake-up cleanup failed", "error", err)
			return
		}
		if n > 0 {
			log.Info("pruned settled wake-ups", "removed", n)
		}
	}

	sweep()
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweep()
		}
	}
}
