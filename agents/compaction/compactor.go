package compaction

import (
	"context"
	"sync"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/session"
)

// Compactor adapts a Strategy to agents.Compactor, so a run can be given one
// without the core knowing about groups, triggers or indexes. It keeps its
// Index between passes, which is why it is a struct: regrouping the whole
// history every turn is work that grows with the thing it shrinks.
type Compactor struct {
	strategy  Strategy
	estimator TokenEstimator

	mu  sync.Mutex
	idx *Index
}

// New returns a Compactor driving strategy. A nil estimator uses CharEstimator.
func New(strategy Strategy, estimator TokenEstimator) *Compactor {
	if estimator == nil {
		estimator = CharEstimator{}
	}
	return &Compactor{strategy: strategy, estimator: estimator}
}

// Compact implements agents.Compactor.
func (c *Compactor) Compact(ctx context.Context, entries []session.Entry) ([]session.Entry, error) {
	if c.strategy == nil || len(entries) == 0 {
		return entries, nil
	}
	// A Compactor may be shared across concurrent runs, and the Index is not
	// safe for that: a torn index is a corrupted context, not a slow one.
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.idx == nil {
		c.idx = NewIndex(entries, c.estimator)
	} else {
		c.idx.Update(entries)
	}
	if _, err := c.strategy.Compact(ctx, c.idx); err != nil {
		return entries, err
	}
	return c.idx.IncludedEntries(), nil
}

// Index exposes the current index, for callers that want to report what was
// dropped. The returned pointer is live: read it, do not mutate it.
func (c *Compactor) Index() *Index {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.idx
}

var _ agents.Compactor = (*Compactor)(nil)

// Checkpoint builds an append-only compaction checkpoint from the compactor's
// current index: what the pass folded away and the stand-ins that render in
// its place. ok=false when nothing was excluded, or when the index no longer
// describes seen. seen is the preceding Compact call's INPUT — the entries the
// pass ran over, not what it produced. What a checkpoint holds: spec §2.5f.
func (c *Compactor) Checkpoint(seen []session.Entry) (session.Entry, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.idx == nil {
		return session.Entry{}, false, nil
	}
	// The index must still describe exactly what the caller's Compact saw; a
	// shared Compactor re-aimed at another session reports nothing (spec §2.5f).
	if n, ok := c.idx.prefixMatches(seen); !ok || n != len(seen) {
		return session.Entry{}, false, nil
	}

	var excluded []string
	var folds []session.CompactionFold
	var prevSummary string
	before := 0
	for i, g := range c.idx.Groups {
		before += g.Tokens
		if !g.Excluded {
			if g.Kind == GroupSummary {
				// The checkpoint this one continues from: recording it lets an
				// updating summarizer see what it is revising.
				if p, err := summaryOf(g); err == nil && p != "" {
					prevSummary = p
				}
			}
			continue
		}
		var replaces []string
		for _, e := range g.Entries {
			if e.ID != "" {
				excluded = append(excluded, e.ID)
				replaces = append(replaces, e.ID)
			}
		}
		if f, ok := foldFor(c.idx.Groups, i, replaces); ok {
			folds = append(folds, f)
		}
	}
	if len(excluded) == 0 {
		return session.Entry{}, false, nil
	}

	e, err := session.NewCompactionEntry(session.CompactionPayload{
		PrevSummary:  prevSummary,
		Folds:        folds,
		ExcludedIDs:  excluded,
		TokensBefore: before,
		TokensAfter:  c.idx.ContextTokens(),
	})
	if err != nil {
		return session.Entry{}, false, err
	}
	return e, true, nil
}

// foldFor turns group i's Replacement into the checkpoint fold that renders in
// its place, anchored before the first surviving entry after it.
func foldFor(groups []*Group, i int, replaces []string) (session.CompactionFold, bool) {
	g := groups[i]
	f := session.CompactionFold{Replaces: replaces}
	for _, re := range g.Replacement {
		if len(re.Item) > 0 {
			f.Items = append(f.Items, re.Item)
		}
	}
	if len(f.Items) == 0 {
		return session.CompactionFold{}, false
	}
	for _, ng := range groups[i+1:] {
		if ng.Excluded {
			continue
		}
		for _, e := range ng.Entries {
			if e.ID != "" {
				f.Before = e.ID
				break
			}
		}
		if f.Before != "" {
			break
		}
	}
	return f, true
}

// summaryOf reads the summary text out of a checkpoint group.
func summaryOf(g *Group) (string, error) {
	for _, e := range g.Entries {
		p, err := e.CompactionPayload()
		if err != nil {
			return "", err
		}
		if p.Summary != "" {
			return p.Summary, nil
		}
	}
	return "", nil
}
