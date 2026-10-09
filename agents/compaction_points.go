package agents

import (
	"context"
	"log/slog"
	"slices"

	"github.com/zzir/agents-go/agents/session"
)

// Compactor decides what a session's history should look like as model
// context: a projection of a log that stays whole, never a deletion — see
// spec §2.5f. Build one with compaction.New.
type Compactor interface {
	Compact(ctx context.Context, entries []session.Entry) ([]session.Entry, error)
}

// CompactionPoint is a set of moments at which a run consults its Compactor.
type CompactionPoint uint8

const (
	// CompactBeforeRun compacts after reading the session, before the first model call.
	CompactBeforeRun CompactionPoint = 1 << iota

	// CompactAtSavePoint compacts at each turn boundary, after the turn's
	// items are persisted and before the next model call.
	CompactAtSavePoint

	// CompactAfterRun compacts once the final output is persisted, shrinking
	// what the next run starts from; a self-compacting storage takes this point.
	CompactAfterRun
)

// Has reports whether p includes q.
func (p CompactionPoint) Has(q CompactionPoint) bool { return p&q != 0 }

// CompactionOptions configures context compaction for a run — see spec §2.5f.
type CompactionOptions struct {
	// Compactor shrinks the context; nil disables compaction.
	Compactor Compactor

	// Points selects when to consult the Compactor; zero means all of them.
	Points CompactionPoint
}

// active reports whether compaction runs at point.
func (c CompactionOptions) active(point CompactionPoint) bool {
	if c.Compactor == nil {
		return false
	}
	return c.Points == 0 || c.Points.Has(point)
}

// compactContext asks the Compactor what the model's context should be,
// returning entries unchanged when compaction is off, inapplicable or failed —
// see spec §2.5f.
func (r *runner) compactContext(ctx context.Context, point CompactionPoint, entries []session.Entry) ([]session.Entry, bool) {
	if !r.opts.Compaction.active(point) {
		return entries, false
	}
	if r.opts.Conversation.Session == nil {
		return entries, false
	}
	if _, ok := r.opts.Conversation.Session.Storage().(session.CompactionAware); ok {
		// A self-compacting storage takes the after-run point instead.
		return entries, false
	}

	// The span opens only when a pass actually runs.
	span := r.trace.StartCompactionSpan(r.agentParentID())
	before := len(entries)
	out, err := r.opts.Compaction.Compactor.Compact(ctx, entries)
	if err != nil {
		span.SetError(err.Error(), nil)
		span.Finish()
		r.log.component("compaction").Warn(ctx, "compaction pass failed; continuing uncompacted",
			slog.String("point", point.String()), slog.String("error", err.Error()))
		RecordDiagnostic(ctx, DiagCompactionFailed, err, map[string]any{"point": point.String()})
		return entries, false
	}
	span.Set("point", point.String())
	span.Set("before_items", before)
	span.Set("after_items", len(out))
	span.Finish()
	// Whole entries, not the count: same count with different content is a legal pass.
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
// whole-entry identity.
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

// recompactAtSavePoint rebuilds the run's context from the persisted log at
// CompactAtSavePoint; ok=false leaves the caller's context alone — see spec §2.5f.
func (r *runner) recompactAtSavePoint(ctx context.Context) (input []InputItem, ok bool, err error) {
	if !r.opts.Compaction.active(CompactAtSavePoint) {
		return nil, false, nil
	}
	sess := r.opts.Conversation.Session
	if sess == nil {
		return nil, false, nil
	}

	// The whole branch is compacted; the history limit bounds the projection
	// afterwards.
	entries, err := sess.ContextEntries(ctx, session.Cursor{})
	if err != nil {
		return nil, false, err
	}
	compacted, changed := r.compactContext(ctx, CompactAtSavePoint, entries)
	if !changed {
		return nil, false, nil
	}

	history, err := session.ProjectEntries(r.historyWindow(compacted), r.opts.Conversation.Projectors)
	if err != nil {
		return nil, false, err
	}
	// Scrubbed like the first turn's history: dropping a group can orphan a
	// tool output.
	return normalizeStoredInput(history), true, nil
}

// historyWindow applies Conversation.Settings to entries: the newest Limit, all
// when unbounded.
func (r *runner) historyWindow(entries []session.Entry) []session.Entry {
	return session.PageEntries(entries, session.Cursor{Limit: -session.ResolveLimit(r.opts.Conversation.Settings)})
}

// CompactionCheckpointer is an optional Compactor capability: describe the
// last pass as an append-only checkpoint entry.
type CompactionCheckpointer interface {
	// Checkpoint records what the preceding Compact call folded away from
	// seen, that call's INPUT; ok=false when nothing was folded or the
	// compactor's state no longer describes seen — see spec §2.5f.
	Checkpoint(seen []session.Entry) (session.Entry, bool, error)
}

// checkpointAfterRun appends the run's compaction as a checkpoint entry and
// reports whether it wrote one.
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
