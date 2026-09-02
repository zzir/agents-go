package bridge

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/zzir/agents-go/cmd/agents-server/internal/attachments"
	"github.com/zzir/agents-go/cmd/agents-server/internal/logging"
	"github.com/zzir/agents-go/cmd/agents-server/internal/settings"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// runEvery runs fn at once and then every period until ctx ends — the shape
// of every maintenance loop here.
func runEvery(ctx context.Context, period time.Duration, fn func()) {
	fn()
	ticker := time.NewTicker(period)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			fn()
		}
	}
}

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

	runEvery(ctx, time.Hour, reap)
}

// RunTraceRetention prunes traces at startup and then once a day: span rows
// older than trace_retention_days (with the blobs of sessions left without
// rows), then the payload of sessions whose newest span is older than
// trace_payload_retention_days — those rows stay, without payload. Either
// setting unset or zero disables its half. It blocks until ctx ends — run it
// in a goroutine.
func RunTraceRetention(ctx context.Context, cfg *settings.Reader, traces *store.TraceStore) {
	log := logging.Ctx(ctx)
	prune := func() {
		now := time.Now().UTC()
		if days := cfg.Int(ctx, settings.KeyTraceRetentionDays); days > 0 {
			n, err := traces.DeleteOlderThan(ctx, now.AddDate(0, 0, -days))
			if err != nil {
				log.Error("trace retention prune failed", "error", err)
			} else if n > 0 {
				log.Info("pruned old trace events", "removed", n, "retention_days", days)
			}
		}
		if days := cfg.Int(ctx, settings.KeyTracePayloadRetentionDays); days > 0 {
			n, err := traces.PrunePayloadBefore(ctx, now.AddDate(0, 0, -days))
			if err != nil {
				log.Error("trace payload prune failed", "error", err)
			} else if n > 0 {
				log.Info("pruned trace payload of idle sessions", "sessions", n, "payload_retention_days", days)
			}
		}
	}

	runEvery(ctx, 24*time.Hour, prune)
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

	runEvery(ctx, time.Hour, sweep)
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

	runEvery(ctx, time.Hour, sweep)
}

// attachmentGrace is how long an uploaded image may wait unsent. Uploads are
// bound the moment a run accepts them, so past this an unbound row is a
// composer draft nobody sent.
const attachmentGrace = 24 * time.Hour

// RunAttachmentReaper collects orphan attachments — uploaded, never accepted
// by a run — hourly: the bucket object first, then the row. That order is
// load-bearing: a row whose object delete failed is retried next sweep,
// while the reverse would leave an unreferenced object forever. The bucket's
// own lifecycle rule is the backstop for objects this best-effort pass
// misses. It blocks until ctx ends — run it in a goroutine.
func RunAttachmentReaper(ctx context.Context, cfg *settings.Reader, atts *store.AttachmentStore) {
	log := logging.Ctx(ctx)
	sweep := func() {
		orphans, err := atts.ListUnboundBefore(ctx, time.Now().UTC().Add(-attachmentGrace))
		if err != nil {
			log.Error("attachment reaper failed", "error", err)
			return
		}
		if len(orphans) == 0 {
			return
		}
		client := attachments.ClientFrom(cfg.S3Config(ctx), cfg.ProxyClient(ctx))
		removed := 0
		for _, a := range orphans {
			// With storage unconfigured the object is unreachable anyway;
			// drop the row so the sentinel degrades cleanly.
			if client != nil {
				if err := client.Delete(ctx, a.Key); err != nil {
					log.Warn("attachment reaper: object delete failed, will retry", "key", a.Key, "error", err)
					continue
				}
			}
			if err := atts.Delete(ctx, a.ID); err != nil {
				log.Warn("attachment reaper: row delete failed, will retry", "id", a.ID, "error", err)
				continue
			}
			removed++
		}
		if removed > 0 {
			log.Info("pruned orphan attachments", "removed", removed)
		}
	}

	runEvery(ctx, time.Hour, sweep)
}

// RunAuditRetention prunes audit events older than days at startup and then
// daily. It blocks until ctx ends — run it in a goroutine.
func RunAuditRetention(ctx context.Context, audit *store.AuditStore, days int) {
	log := logging.Ctx(ctx)
	prune := func() {
		n, err := audit.DeleteOlderThan(ctx, time.Now().UTC().AddDate(0, 0, -days))
		if err != nil {
			log.Error("audit retention prune failed", "error", err)
			return
		}
		if n > 0 {
			log.Info("pruned audit events", "removed", n, "retention_days", days)
		}
	}

	runEvery(ctx, 24*time.Hour, prune)
}
