package agents

import (
	"sync"
	"sync/atomic"
)

// RunControl influences a run in flight: stop, injection, pending. Safe from
// another goroutine, including before ranging begins — see spec §2.11b.
type RunControl interface {
	// StopAfterTurn ends the run cleanly once the in-flight turn, tools and
	// session save included, finishes — see spec §2.3c.
	StopAfterTurn()

	// Steer injects input (a string or []InputItem) and forces another turn
	// even if the agent was about to finish.
	Steer(input any) error

	// NextTurn injects input at the next turn boundary, if the run takes one;
	// it never extends the run.
	NextTurn(input any) error

	// FollowUp queues input for after the final output, continuing the same run.
	FollowUp(input any) error

	// Pending reports queued input the run has not consumed.
	Pending() PendingInput
}

// PendingInput is queued input a run has not consumed.
type PendingInput struct {
	Steer    []InputItem
	NextTurn []InputItem
	FollowUp []InputItem
}

// Empty reports whether nothing is queued.
func (p PendingInput) Empty() bool {
	return len(p.Steer) == 0 && len(p.NextTurn) == 0 && len(p.FollowUp) == 0
}

// injectKind tags a queued injection with the method that queued it — see spec §2.11b.
type injectKind uint8

const (
	injectSteer injectKind = iota
	injectNextTurn
	injectFollowUp
)

// pendingEntry is one queued injection: what arrived, through which method,
// and when relative to its neighbors.
type pendingEntry struct {
	kind  injectKind
	items []InputItem
	seq   uint64
}

// runControl is the concrete RunControl. The caller holds it from the moment
// Run returns, so every field is written by the loop and read concurrently.
type runControl struct {
	stopAfterTurn atomic.Bool

	mu sync.Mutex

	// pending is the arrival-ordered injection queue; inFlight holds what the
	// current attempt took but has not made durable — spec §2.11b.
	pending  []pendingEntry
	inFlight []pendingEntry
	seq      uint64 // arrival stamp; keeps rollback merges in arrival order
	restored bool   // a paused run's PendingInput seeds the queue once per control
}

func newRunControl() *runControl { return &runControl{} }

func (c *runControl) StopAfterTurn() { c.stopAfterTurn.Store(true) }

func (c *runControl) stopRequested() bool { return c.stopAfterTurn.Load() }

// Steer implements RunControl.
func (c *runControl) Steer(input any) error { return c.enqueue(injectSteer, input) }

// NextTurn implements RunControl.
func (c *runControl) NextTurn(input any) error { return c.enqueue(injectNextTurn, input) }

// FollowUp implements RunControl.
func (c *runControl) FollowUp(input any) error { return c.enqueue(injectFollowUp, input) }

func (c *runControl) enqueue(kind injectKind, input any) error {
	items, err := normalizeInput(input)
	if err != nil {
		return err
	}
	if len(items) == 0 {
		return nil
	}
	c.mu.Lock()
	c.seq++
	c.pending = append(c.pending, pendingEntry{kind: kind, items: items, seq: c.seq})
	c.mu.Unlock()
	return nil
}

// Pending implements RunControl.
func (c *runControl) Pending() PendingInput {
	c.mu.Lock()
	defer c.mu.Unlock()
	var p PendingInput
	for _, e := range c.pending {
		switch e.kind {
		case injectSteer:
			p.Steer = append(p.Steer, e.items...)
		case injectNextTurn:
			p.NextTurn = append(p.NextTurn, e.items...)
		case injectFollowUp:
			p.FollowUp = append(p.FollowUp, e.items...)
		}
	}
	return p
}

// takeTurnInput drains what the save point delivers — steer and next-turn
// input, in arrival order — into the in-flight set.
func (c *runControl) takeTurnInput() []InputItem {
	return c.take(func(k injectKind) bool { return k == injectSteer || k == injectNextTurn })
}

// takeContinuation drains what may extend a run at its final output: a late
// steer and any follow-up. NextTurn is absent by design (spec §2.11b).
func (c *runControl) takeContinuation() []InputItem {
	return c.take(func(k injectKind) bool { return k == injectSteer || k == injectFollowUp })
}

// take moves the accepted pending entries to the in-flight set and returns
// their items in arrival order; commit/rollback settle delivery.
func (c *runControl) take(want func(injectKind) bool) []InputItem {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []InputItem
	keep := c.pending[:0]
	for _, e := range c.pending {
		if want(e.kind) {
			c.inFlight = append(c.inFlight, e)
			out = append(out, e.items...)
			continue
		}
		keep = append(keep, e)
	}
	c.pending = keep
	return out
}

// inFlightMark is where the in-flight set stands, for discardInFlightSince.
func (c *runControl) inFlightMark() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.inFlight)
}

// discardInFlightSince drops the entries taken after mark: input a guardrail
// refused is consumed, never redelivered (spec §2.11b).
func (c *runControl) discardInFlightSince(mark int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if mark < len(c.inFlight) {
		c.inFlight = c.inFlight[:mark]
	}
}

// commitInjected marks every in-flight injection delivered: the items have a
// durable home (spec §2.11b), and no retry re-delivers them.
func (c *runControl) commitInjected() {
	c.mu.Lock()
	c.inFlight = nil
	c.mu.Unlock()
}

// rollbackInjected returns in-flight injections to the queue in arrival order,
// so the next attempt delivers what the failed one never made durable.
func (c *runControl) rollbackInjected() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.inFlight) == 0 {
		return
	}
	merged := make([]pendingEntry, 0, len(c.inFlight)+len(c.pending))
	i, j := 0, 0
	for i < len(c.inFlight) && j < len(c.pending) {
		if c.inFlight[i].seq < c.pending[j].seq {
			merged = append(merged, c.inFlight[i])
			i++
		} else {
			merged = append(merged, c.pending[j])
			j++
		}
	}
	merged = append(merged, c.inFlight[i:]...)
	merged = append(merged, c.pending[j:]...)
	c.pending, c.inFlight = merged, nil
}

// restore seeds the queue from a paused run's state, once per control; the
// transaction, not reseeding, makes input survive a retry (spec §2.11b).
func (c *runControl) restore(p PendingInput) {
	if p.Empty() {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.restored {
		return
	}
	c.restored = true
	for _, s := range []struct {
		kind  injectKind
		items []InputItem
	}{
		{injectSteer, p.Steer},
		{injectNextTurn, p.NextTurn},
		{injectFollowUp, p.FollowUp},
	} {
		if len(s.items) == 0 {
			continue
		}
		c.seq++
		c.pending = append(c.pending, pendingEntry{kind: s.kind, items: s.items, seq: c.seq})
	}
}
