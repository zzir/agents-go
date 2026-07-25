package compaction

import (
	"context"
	"sync"

	"github.com/zzir/agents-go/agents"
)

// Compactor adapts a Strategy to agents.Compactor, so a run can be given one
// without the core package knowing anything about groups, triggers or indexes.
//
// It keeps its Index between passes. That is the whole reason it is a struct
// rather than a function: a conversation that regroups its entire history on
// every turn does work proportional to the thing it is trying to shrink, and
// does it again on the next turn.
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
func (c *Compactor) Compact(ctx context.Context, entries []agents.SessionEntry) ([]agents.SessionEntry, error) {
	if c.strategy == nil || len(entries) == 0 {
		return entries, nil
	}
	// One run's passes are sequential, but a Compactor may be shared across
	// concurrent runs — an application that configures one and reuses it. The
	// Index is not safe for that, and a torn index is a corrupted context
	// rather than a slow one.
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
