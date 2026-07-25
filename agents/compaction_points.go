package agents

import "context"

// Compactor decides what a session's history should look like as model context.
//
// It takes the entries the run would otherwise send and returns the ones it
// should. It never deletes: the log stays whole and a compactor's answer is a
// projection of it, which is what lets a session be forked, replayed and read
// concurrently while its context shrinks.
//
// The interface lives here rather than in agents/compaction so the core can
// name it without importing the grouping and strategy machinery — the same
// reason a Model is an interface here and an implementation elsewhere. Build
// one with compaction.New.
type Compactor interface {
	Compact(ctx context.Context, entries []SessionEntry) ([]SessionEntry, error)
}

// CompactionPoint is a set of moments at which a run consults its Compactor.
type CompactionPoint uint8

const (
	// CompactBeforeRun compacts after reading the session, before the first
	// model call. It is what keeps a long conversation from blowing the
	// context window on its very first turn.
	CompactBeforeRun CompactionPoint = 1 << iota

	// CompactAtSavePoint compacts at each turn boundary — after the turn's
	// items are persisted, before the next model call.
	//
	// It is the point the old design lacked, and the one that matters for
	// agentic work: a run that calls thirty tools overruns its window inside a
	// single run, long before the run-level pass would ever get to look.
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
// drop needs the model (to summarize), the usage numbers (to measure) and the
// context window (to compare against), and all three belong to the run. A
// session decorator holding a summarization model was a shape inherited from
// elsewhere, not one this SDK needs.
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
func (r *runner) compactContext(ctx context.Context, point CompactionPoint, entries []SessionEntry) []SessionEntry {
	if !r.opts.Compaction.active(point) {
		return entries
	}
	if r.opts.Conversation.Session == nil {
		// Without a session there is no history to shrink: the caller's input
		// is all the context there is, and dropping part of what they just
		// asked for is not compaction.
		return entries
	}
	if _, ok := r.opts.Conversation.Session.Storage().(CompactionAware); ok {
		// The storage compacts itself (a server-side compact API, say).
		// Running both would compact a history that is already shrinking under
		// a different policy.
		return entries
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
		return entries
	}
	if span != nil {
		span.Set("point", point.String())
		span.Set("entries_before", before)
		span.Set("entries_after", len(out))
		span.Finish()
	}
	return out
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
// Rebuilding rather than editing the in-flight item list is what makes the
// append-only model pay off. The log is the truth; the context is a projection
// of it, and a projection is cheap to recompute and impossible to get out of
// step with what was stored. Trying to splice a compacted result into the
// running []RunItem would mean converting entries the other way, which no
// folded summary survives.
//
// It reports ok=false when nothing applies, leaving the caller's context alone.
func (r *runner) recompactAtSavePoint(ctx context.Context) (input []TResponseInputItem, ok bool, err error) {
	if !r.opts.Compaction.active(CompactAtSavePoint) {
		return nil, false, nil
	}
	sess := r.opts.Conversation.Session
	if sess == nil {
		return nil, false, nil
	}

	cur := Cursor{Limit: -resolveSessionLimit(r.opts.Conversation.Settings)}
	entries, err := sess.ContextEntries(ctx, cur)
	if err != nil {
		return nil, false, err
	}
	compacted := r.compactContext(ctx, CompactAtSavePoint, entries)
	if len(compacted) == len(entries) {
		// Nothing was dropped, so rebuilding would produce the context the run
		// already has. Skipping keeps the common no-op turn free.
		return nil, false, nil
	}

	history, err := ProjectEntries(compacted, r.opts.Conversation.Projectors)
	if err != nil {
		return nil, false, err
	}
	// Scrub before sending, exactly as the run's first turn does: dropping a
	// group can leave a tool output whose call is gone, which the Responses API
	// rejects.
	return normalizeStoredInput(history), true, nil
}
