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

// CompactionOptions configures context compaction for a run.
//
// Compaction is a RUN-level concern, not a session-level one: deciding what to
// drop needs the model, the usage numbers and the context window, all of which
// belong to the run.
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

// compactContext asks the Compactor what the model's context should be.
//
// It returns entries unchanged whenever compaction is off, does not apply at
// this point, or failed — a pass is housekeeping, and the context it was
// shrinking is still valid, so there is no error for a caller to handle.
//
// Server-managed conversation state needs no thought here: UsePreviousResponseID
// and ConversationID already refuse to combine with a local Session, so a run
// whose history the server holds has no entries for a compactor to look at.
func (r *runner) compactContext(ctx context.Context, point CompactionPoint, entries []session.Entry) ([]session.Entry, bool) {
	if !r.opts.Compaction.active(point) {
		return entries, false
	}
	if r.opts.Conversation.Session == nil {
		// Without a session there is no history to shrink: the caller's input
		// is all the context there is, and dropping part of what they just
		// asked for is not compaction.
		return entries, false
	}
	if _, ok := r.opts.Conversation.Session.Storage().(session.CompactionAware); ok {
		// The storage compacts itself (a server-side compact API, say).
		// Running both would compact a history that is already shrinking under
		// a different policy.
		return entries, false
	}

	// The span opens only when a pass actually runs, so no-op turns — the vast
	// majority — leave the trace alone.
	span := r.trace.StartCompactionSpan(r.agentParentID())
	before := len(entries)
	out, err := r.opts.Compaction.Compactor.Compact(ctx, entries)
	if err != nil {
		// Aborting would turn a housekeeping problem into a failed run.
		if span != nil {
			span.SetError(err.Error(), nil)
			span.Finish()
		}
		r.log.component("compaction").Warn(ctx, "compaction pass failed; continuing uncompacted",
			slog.String("point", point.String()), slog.String("error", err.Error()))
		RecordDiagnostic(ctx, DiagCompactionFailed, err, map[string]any{"point": point.String()})
		return entries, false
	}
	if span != nil {
		span.Set("point", point.String())
		span.Set("entries_before", before)
		span.Set("entries_after", len(out))
		span.Finish()
	}
	// Length is not the test. A compactor may return the same COUNT with
	// different content — one summary standing in for one entry is a legal
	// pass — and treating that as a no-op would silently discard it.
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
// identity rather than by size.
//
// Every field takes part, not just id and item: a strategy that rewrites only a
// payload (a checkpoint's own body) has still changed the context, and a custom
// projector may read any field.
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
// persisted, so the session log is complete and the run can rebuild its context
// from it.
//
// It rebuilds from the log rather than editing the in-flight item list: the
// context is a projection, cheap to recompute and impossible to get out of step
// with what was stored. It reports ok=false when nothing applies, leaving the
// caller's context alone.
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
		// The pass altered nothing, so rebuilding would produce the context
		// the run already has. Skipping keeps the common no-op turn free.
		return nil, false, nil
	}

	history, err := session.ProjectEntries(compacted, r.opts.Conversation.Projectors)
	if err != nil {
		return nil, false, err
	}
	// Scrub before sending, exactly as the run's first turn does: dropping a
	// group can leave a tool output whose call is gone, which the Responses API
	// rejects.
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
//
// It reports whether it wrote one. Failure is not fatal — a missing checkpoint
// costs the next run one more compaction pass.
func (r *runner) checkpointAfterRun(ctx context.Context) bool {
	if !r.opts.Compaction.active(CompactAfterRun) || r.opts.Conversation.Session == nil {
		return false
	}
	cp, ok := r.opts.Compaction.Compactor.(CompactionCheckpointer)
	if !ok {
		return false
	}

	// Compact once more over the whole persisted history: the passes during the
	// run happened before this turn's items existed.
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
			// Finished, or the error never leaves this process: a span is
			// exported on Finish and an abandoned one reaches no processor.
			span.Finish()
		}
		return false
	}
	if err := r.opts.Conversation.Session.Append(ctx, entry); err != nil {
		return false
	}
	return true
}
