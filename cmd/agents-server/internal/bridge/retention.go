package bridge

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/rs/zerolog"

	"github.com/zzir/agents-go/cmd/agents-server/internal/settings"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// RunApprovalReaper expires pending tool approvals that have gone unanswered
// past the TTL. On expiry it drops the record and writes a session annotation
// so the timeout is visible instead of silently vanishing. It runs at startup
// and hourly until ctx ends — run it in a goroutine.
func RunApprovalReaper(ctx context.Context, cfg *settings.Reader, approvals *store.PendingApprovalStore, entries *store.EntryStore, tasks *store.TaskStore, announce func(ctx context.Context, taskID string)) {
	log := zerolog.Ctx(ctx)
	reap := func() {
		ttl := cfg.Int(ctx, settings.KeyApprovalTTLMinutes)
		if ttl <= 0 {
			return // expiry disabled
		}
		cutoff := time.Now().UTC().Add(-time.Duration(ttl) * time.Minute)
		expired, err := approvals.ListOlderThan(ctx, cutoff)
		if err != nil {
			log.Error().Err(err).Msg("approval reaper failed")
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
					log.Warn().Err(terr).Str("run_id", p.RunID).Msg("expiring an approval: its task could not be read; kept for the next round")
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
					log.Warn().Err(err).Str("run_id", p.RunID).Msg("expiring an approval; kept for the next round")
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
				log.Warn().Err(derr).Str("run_id", p.RunID).Msg("expiring an approval; kept for the next round")
				continue
			}
			if !claimed {
				continue // a decision took it first
			}
			// The banner goes to the session the approval was filed on — a
			// task's or a step's hidden child session, whose transcript is
			// where the pause happened.
			if ref, rerr := entries.RefFor(ctx, p.SessionID); rerr != nil {
				log.Warn().Err(rerr).Str("session_id", p.SessionID).
					Msg("cannot record an approval-timeout banner")
			} else {
				banner := "Tool approval timed out after " + strconv.Itoa(ttl) + " minutes; the run was terminated."
				if p.Kind == store.ApprovalKindStep {
					banner = "Step approval timed out after " + strconv.Itoa(ttl) + " minutes; the workflow was cancelled."
				}
				_ = entries.AppendAnnotation(ctx, ref, p.RunID, banner)
			}
			log.Info().Str("run_id", p.RunID).Str("session_id", p.SessionID).Msg("expired pending approval")
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
	log := zerolog.Ctx(ctx)
	prune := func() {
		days := cfg.Int(ctx, settings.KeyTraceRetentionDays)
		if days <= 0 {
			return // unset or zero — retention disabled
		}
		cutoff := time.Now().UTC().AddDate(0, 0, -days)
		n, err := traces.DeleteOlderThan(ctx, cutoff)
		if err != nil {
			log.Error().Err(err).Msg("trace retention prune failed")
			return
		}
		if n > 0 {
			log.Info().Int64("removed", n).Int("retention_days", days).Msg("pruned old trace events")
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
