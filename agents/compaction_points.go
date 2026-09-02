package agents

import (
	"context"
	"log/slog"
	"slices"

	"github.com/zzir/agents-go/agents/session"
)

// Compactor decides what a session's history should look like as model context.
//
// It takes the entries the run would otherwise send and returns the ones it
// should. It never deletes — the answer is a projection of a log that stays
// whole, which is what lets a session be forked, replayed and read concurrently
// while its context shrinks. Build one with compaction.New.
type Compactor interface {
	Compact(ctx context.Context, entries []session.Entry) ([]session.Entry, error)
}

// CompactionPoint is a set of moments at which a run consults its Compactor.
type CompactionPoint uint8

const (
	// CompactBeforeRun compacts after reading the session, before the first
	// model call. It is what keeps a long conversation from blowing the
	// context window on its very first turn.
	CompactBeforeRun CompactionPoint = 1 << iota

	// CompactAtSavePoint compacts at each turn boundary — after the turn's
	// items are persisted, before the next model call. It is the point that
	// matters for agentic work: a run that calls thirty tools overruns its
	// window inside a single run, long before a run-level pass would look.
	CompactAtSavePoint

	// CompactAfterRun compacts once the run's final output is produced and
	// persisted. It changes nothing about this run — it shrinks what the next
	// one starts from.
	//
	// This is where a self-compacting STORAGE gets its turn (a server-side
	// compact API); a Compactor records its result here as a checkpoint.
	CompactAfterRun
)

// Has reports whether p includes q.
func (p CompactionPoint) Has(q CompactionPoint) bool { return p&q != 0 }

// CompactionOptions configures context compaction for a run — a run-level
// concern, since what to drop depends on the model and its context window
// (spec §2.5f).
type CompactionOptions struct {
	// Compactor shrinks the context. Nil disables compaction entirely.
	Compactor Compactor

	// Points selects when to consult the Compactor. The zero value means all
	// of them, which is the useful default — a caller who wants no compaction
	// leaves Compactor nil rather than clearing this.
	Points CompactionPoint
}

// active reports whether compaction runs at point.
func (c CompactionOptions) active(point CompactionPoint) bool {
	if c.Compactor == nil {
		return false
	}
	return c.Points == 0 || c.Points.Has(point)
}

// compactContext asks the Compactor what the model's context should be. It
// returns entries unchanged whenever compaction is off, does not apply at this
// point, or failed — a pass never fails a run (spec §2.5f).
func (r *runner) compactContext(ctx context.Context, point CompactionPoint, entries []session.Entry) ([]session.Entry, bool) {
	if !r.opts.Compaction.active(point) {
		return entries, false
	}
	if r.opts.Conversation.Session == nil {
		// No history to shrink: the caller's input is all the context there is.
		return entries, false
	}
	if _, ok := r.opts.Conversation.Session.Storage().(session.CompactionAware); ok {
		// A self-compacting storage takes the after-run point instead; the two
		// never both run on one session.
		return entries, false
	}

	// The span opens only when a pass actually runs, so no-op turns leave the
	// trace alone.
	span := r.trace.StartCompactionSpan(r.agentParentID())
	before := len(entries)
	out, err := r.opts.Compaction.Compactor.Compact(ctx, entries)
	if err != nil {
		// Aborting would turn a housekeeping problem into a failed run.
		span.SetError(err.Error(), nil)
		span.Finish()
		r.log.component("compaction").Warn(ctx, "compaction pass failed; continuing uncompacted",
			slog.String("point", point.String()), slog.String("error", err.Error()))
		RecordDiagnostic(ctx, DiagCompactionFailed, err, map[string]any{"point": point.String()})
		return entries, false
	}
	span.Set("point", point.String())
	span.Set("entries_before", before)
	span.Set("entries_after", len(out))
	span.Finish()
	// Whole entries, not the count: same count with different content is a
	// legal pass (spec §2.5f).
	changed := changedEntries(entries, out)
	if changed {
		r.log.component("compaction").Info(ctx, "context compacted",
			slog.String("point", point.String()),
			slog.Int("entries_before", before),
			slog.Int("entries_after", len(out)))
	}
	return out, changed
}

// changedEntries reports whether a compaction pass altered the context, by
// whole-entry identity rather than by size.
func changedEntries(before, after []session.Entry) bool {
	return !slices.EqualFunc(before, after, session.Entry.Equal)
}

// String names the point, for traces and logs.
func (p CompactionPoint) String() string {
	switch p {
	case CompactBeforeRun:
		return "before_run"
	case CompactAtSavePoint:
		return "save_point"
	case CompactAfterRun:
		return "after_run"
	default:
		return "multiple"
	}
}

// recompactAtSavePoint is the CompactAtSavePoint point: the turn's items are
// persisted, so the run rebuilds its context from the log (spec §2.5f). It
// reports ok=false when nothing applies, leaving the caller's context alone.
func (r *runner) recompactAtSavePoint(ctx context.Context) (input []InputItem, ok bool, err error) {
	if !r.opts.Compaction.active(CompactAtSavePoint) {
		return nil, false, nil
	}
	sess := r.opts.Conversation.Session
	if sess == nil {
		return nil, false, nil
	}

	cur := session.Cursor{Limit: -session.ResolveLimit(r.opts.Conversation.Settings)}
	entries, err := sess.ContextEntries(ctx, cur)
	if err != nil {
		return nil, false, err
	}
	compacted, changed := r.compactContext(ctx, CompactAtSavePoint, entries)
	if !changed {
		return nil, false, nil
	}

	history, err := session.ProjectEntries(compacted, r.opts.Conversation.Projectors)
	if err != nil {
		return nil, false, err
	}
	// Scrubbed like the first turn's history: dropping a group can orphan a
	// tool output.
	return normalizeStoredInput(history), true, nil
}

// CompactionCheckpointer is an optional Compactor capability: describe the last
// pass as an append-only checkpoint entry. It is optional — a compactor that
// only reshapes context in memory has nothing durable to record.
type CompactionCheckpointer interface {
	// Checkpoint returns the entry recording what the compactor folded away
	// from exactly this context — the entries the caller's own preceding Compact
	// call saw. ok is false when nothing was folded, and when the compactor's
	// shared state no longer describes these entries: a concurrent run may have
	// re-aimed it, and recording that here would leak another conversation's
	// exclusions into this log (spec §2.5f).
	//
	// seen is that preceding Compact call's INPUT, not its result.
	Checkpoint(seen []session.Entry) (session.Entry, bool, error)
}

// checkpointAfterRun records the run's compaction as an append-only checkpoint,
// so the next run starts from the shorter context instead of recomputing it.
// Only a CompactionCheckpointer runs the after-run pass at all. It reports
// whether it wrote one; failure costs the next run one more pass, nothing more.
func (r *runner) checkpointAfterRun(ctx context.Context) bool {
	if !r.opts.Compaction.active(CompactAfterRun) || r.opts.Conversation.Session == nil {
		return false
	}
	cp, ok := r.opts.Compaction.Compactor.(CompactionCheckpointer)
	if !ok {
		return false
	}

	// Over the whole persisted history: the run's passes predate this turn.
	entries, err := r.opts.Conversation.Session.ContextEntries(ctx, session.Cursor{})
	if err != nil {
		return false
	}
	if _, changed := r.compactContext(ctx, CompactAfterRun, entries); !changed {
		return false
	}

	entry, ok, err := cp.Checkpoint(entries)
	if err != nil || !ok {
		if err != nil {
			span := r.trace.StartCompactionSpan(r.agentParentID())
			span.SetError(err.Error(), nil)
			span.Finish() // a span is exported on Finish
		}
		return false
	}
	if err := r.opts.Conversation.Session.Append(ctx, entry); err != nil {
		return false
	}
	return true
}
