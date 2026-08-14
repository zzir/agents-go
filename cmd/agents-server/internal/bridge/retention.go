package bridge

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// defaultApprovalTTLMinutes bounds how long a run may sit awaiting approval
// before it is expired. Overridable via the approval_ttl_minutes setting; 0
// disables expiry.
const defaultApprovalTTLMinutes = 24 * 60

// RunApprovalReaper expires pending tool approvals that have gone unanswered
// past the TTL. On expiry it drops the record and writes a session annotation
// so the timeout is visible instead of silently vanishing. It runs at startup
// and hourly until ctx ends — run it in a goroutine.
// onExpired, when set, is told the session an expired approval belonged to, so
// a caller that owns other work on it (a workflow execution) can end that too.
func RunApprovalReaper(ctx context.Context, settings *store.SettingStore, approvals *store.PendingApprovalStore, entries *store.EntryStore, tasks *store.TaskStore, wakeups *store.WakeupStore, onExpired func(context.Context, string)) {
	log := zerolog.Ctx(ctx)
	reap := func() {
		ttl := defaultApprovalTTLMinutes
		if st, err := settings.Get(ctx, "approval_ttl_minutes"); err == nil {
			if v, err := strconv.Atoi(strings.TrimSpace(st.Value)); err == nil {
				ttl = v
			}
		}
		if ttl <= 0 {
			return // expiry disabled
		}
		cutoff := time.Now().UTC().Add(-time.Duration(ttl) * time.Minute)
		expired, err := approvals.DeleteOlderThan(ctx, cutoff)
		if err != nil {
			log.Error().Err(err).Msg("approval reaper failed")
			return
		}
		for _, p := range expired {
			ref, rerr := entries.RefFor(ctx, p.SessionID)
			if rerr != nil {
				// The session is gone, or unreadable: there is nowhere to put
				// the banner, and guessing at a scope is how one lands where
				// nothing reads it.
				log.Warn().Err(rerr).Str("session_id", p.SessionID).
					Msg("cannot record an approval-timeout banner")
				continue
			}
			_ = entries.AppendAnnotation(ctx, ref, p.RunID,
				"Tool approval timed out after "+strconv.Itoa(ttl)+" minutes; the run was terminated.")
			// A background task's approval expiring must finalize the task row
			// too — otherwise it is a zombie stuck at input_required that no
			// stop or approve can ever advance.
			if tasks != nil {
				if task, err := tasks.ByChildSession(ctx, p.SessionID); err == nil {
					// Against p.RunID — the attempt this expired approval
					// belongs to — NEVER the row's current run id: after a
					// crash + FailOrphans + retry, the row names the retry's
					// run, and finalizing against it would cancel a healthy
					// new attempt because an approval from a previous life
					// expired. The run-id predicate makes the stale case a
					// silent no-op instead.
					if won, _ := tasks.Finalize(ctx, task.ID, p.RunID, "cancelled",
						"approval expired after "+strconv.Itoa(ttl)+" minutes", "", nil); won {
						// Cancellations owe no wake-up; the timeout annotation
						// above is the parent's record.
						if wakeups != nil {
							if err := wakeups.CancelFor(ctx, WakeKindTask, task.ID, p.RunID); err != nil {
								log.Warn().Err(err).Str("task_id", task.ID).Msg("cancelling an expired task's wake-up")
							}
						}
					}
				}
			}
			// A workflow's step waiting on this approval will never resume, so
			// the execution has to be ended too — otherwise it stays running
			// forever with nothing left that could finish it.
			if onExpired != nil {
				onExpired(ctx, p.SessionID)
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
func RunTraceRetention(ctx context.Context, settings *store.SettingStore, traces *store.TraceStore) {
	log := zerolog.Ctx(ctx)
	prune := func() {
		st, err := settings.Get(ctx, "trace_retention_days")
		if err != nil {
			return // unset — retention disabled
		}
		days, err := strconv.Atoi(strings.TrimSpace(st.Value))
		if err != nil || days <= 0 {
			return
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
