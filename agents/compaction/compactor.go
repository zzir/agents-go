package compaction

import (
	"context"
	"sync"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/session"
)

// Compactor adapts a Strategy to agents.Compactor, keeping its Index between
// passes so the history is not regrouped every turn.
type Compactor struct {
	strategy  Strategy
	estimator TokenEstimator

	// ResetSummary, when set, is what a Reset carries into the fresh context
	// (memory.Snapshot, say); nil resets bare.
	ResetSummary func(ctx context.Context) (string, error)

	mu  sync.Mutex
	idx *Index
	// reset marks the index as holding a reset the next checkpoint records.
	reset bool
}

// New returns a Compactor driving strategy. A nil estimator uses CharEstimator.
func New(strategy Strategy, estimator TokenEstimator) *Compactor {
	if estimator == nil {
		estimator = CharEstimator{}
	}
	return &Compactor{strategy: strategy, estimator: estimator}
}

// Compact implements agents.Compactor. Without a strategy nothing folds, but an
// index a Reset built is still served so the reset reaches the checkpoint.
func (c *Compactor) Compact(ctx context.Context, entries []session.Entry) ([]session.Entry, error) {
	if len(entries) == 0 {
		return entries, nil
	}
	// A Compactor may be shared across concurrent runs; the Index is not safe for that.
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.strategy == nil && c.idx == nil {
		return entries, nil
	}
	if c.idx == nil {
		c.idx = NewIndex(entries, c.estimator)
	} else {
		c.idx.Update(entries)
	}
	if c.strategy != nil {
		if _, err := c.strategy.Compact(ctx, c.idx); err != nil {
			return entries, err
		}
	}
	return c.idx.IncludedEntries(), nil
}

// Reset implements agents.ContextResetter: every group but the system ones
// and the newest user message is excluded, the earlier checkpoints included,
// and ResetSummary's text stands in front of what survives (spec §2.5i).
func (c *Compactor) Reset(ctx context.Context, entries []session.Entry) ([]session.Entry, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.idx == nil {
		c.idx = NewIndex(entries, c.estimator)
	} else {
		c.idx.Update(entries)
	}
	summary := ""
	if c.ResetSummary != nil {
		text, err := c.ResetSummary(ctx)
		if err != nil {
			return entries, err
		}
		summary = text
	}
	keep := -1
	for i := len(c.idx.Groups) - 1; i >= 0; i-- {
		if c.idx.Groups[i].Kind == GroupUser && !c.idx.Groups[i].Excluded {
			keep = i
			break
		}
	}
	// Every group but the kept ones folds, earlier exclusions included: this
	// reset's summary supersedes their stand-ins.
	first := -1
	for i, g := range c.idx.Groups {
		if i == keep || g.Kind == GroupSystem {
			continue
		}
		g.Excluded = true
		g.ExcludeReason = "reset"
		g.Replacement = nil
		if first < 0 {
			first = i
		}
	}
	if first >= 0 && summary != "" {
		e, err := foldedEntry(session.SummaryMarker + "\n\n" + summary)
		if err != nil {
			return entries, err
		}
		c.idx.Groups[first].Replacement = []session.Entry{e}
	}
	c.reset = first >= 0
	return c.idx.IncludedEntries(), nil
}

// Index exposes the current index; the pointer is live, read it, do not mutate it.
func (c *Compactor) Index() *Index {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.idx
}

var _ agents.Compactor = (*Compactor)(nil)

// Checkpoint builds a compaction checkpoint from the current index; seen is the
// preceding Compact call's INPUT, and ok=false when nothing was excluded or the
// index no longer describes seen — see spec §2.5f.
func (c *Compactor) Checkpoint(seen []session.Entry) (session.Entry, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.idx == nil {
		return session.Entry{}, false, nil
	}
	// A shared Compactor re-aimed at another session reports nothing (spec §2.5f).
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
				// The checkpoint this one continues from, so an updating
				// summarizer sees what it revises.
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
		Reset:        c.reset,
	})
	if err != nil {
		return session.Entry{}, false, err
	}
	c.reset = false
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
