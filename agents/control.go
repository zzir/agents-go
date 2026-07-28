package agents

import (
	"sync"
	"sync/atomic"
)

// RunPhase says what a run is doing right now. It is advisory — useful for a
// progress indicator during a long silence — and a run moves between phases
// many times per turn.
type RunPhase int32

const (
	// PhaseIdle is before the first turn and after the last.
	PhaseIdle RunPhase = iota
	// PhaseGuardrails is running input or output guardrails.
	PhaseGuardrails
	// PhaseModelCall is waiting on the model.
	PhaseModelCall
	// PhaseToolExecution is running tools.
	PhaseToolExecution
	// PhasePersisting is writing the turn to the session.
	PhasePersisting
	// PhaseCompaction is compacting session history.
	PhaseCompaction
)

func (p RunPhase) String() string {
	switch p {
	case PhaseIdle:
		return "idle"
	case PhaseGuardrails:
		return "guardrails"
	case PhaseModelCall:
		return "model"
	case PhaseToolExecution:
		return "tools"
	case PhasePersisting:
		return "persisting"
	case PhaseCompaction:
		return "compaction"
	}
	return "unknown"
}

// RunControl influences a run while it is in flight. Run returns one alongside
// the stream; it is safe to use from another goroutine, including before
// ranging has begun.
type RunControl interface {
	// StopAfterTurn asks the run to stop once the in-flight turn finishes —
	// tools and session save included — ending cleanly with no error before the
	// next turn begins.
	//
	// This is not the same as abandoning the stream. Breaking out of the range
	// loop stops the run where it stands, mid-turn, with that turn's work
	// unfinished; cancelling the context does the same, harder. StopAfterTurn
	// is the one that leaves the session consistent.
	StopAfterTurn()

	// Phase reports what the run is doing right now.
	Phase() RunPhase

	// CurrentAgent returns the agent handling the turn in progress, or nil
	// before the first turn starts.
	CurrentAgent() *Agent

	// CurrentTurn returns the 1-based number of the turn in progress, or 0
	// before the first turn starts.
	CurrentTurn() int

	// Steer injects input into the running run and forces another turn even if
	// the agent was about to produce its final output.
	//
	// It is "change course": the user says something while the agent is
	// working, and it must reach the model whether or not the agent thought it
	// was finished. Input is a string or []TResponseInputItem.
	Steer(input any) error

	// NextTurn injects input at the next turn boundary, if the run takes one.
	//
	// It is "while you are at it": the input rides along with a turn the run
	// was going to take anyway. Unlike Steer it never extends the run, so a run
	// that is finishing simply finishes. Whatever it did not consume is
	// reported by Pending.
	NextTurn(input any) error

	// FollowUp queues input for after the run's final output, continuing the
	// same run with it rather than ending.
	//
	// It is "and then": the current exchange finishes on its own terms —
	// answer produced, turn saved — and the next one starts from it, in the
	// same run, so the trace, the usage total and the session stay one thing.
	FollowUp(input any) error

	// Pending reports queued input the run has not consumed. It is how a
	// caller learns that a NextTurn arrived too late to be delivered, instead
	// of the input vanishing.
	Pending() PendingInput
}

// PendingInput is queued input a run has not consumed.
type PendingInput struct {
	Steer    []TResponseInputItem
	NextTurn []TResponseInputItem
	FollowUp []TResponseInputItem
}

// Empty reports whether nothing is queued.
func (p PendingInput) Empty() bool {
	return len(p.Steer) == 0 && len(p.NextTurn) == 0 && len(p.FollowUp) == 0
}

// runControl is the concrete RunControl. It is created before the run starts —
// the caller holds it from the moment Run returns and may read it before
// ranging — so every field is written by the run loop and read concurrently.
type runControl struct {
	stopAfterTurn atomic.Bool
	phase         atomic.Int32

	mu           sync.Mutex
	currentAgent *Agent
	currentTurn  int

	// The three injection queues. They are separate rather than one queue with
	// a mode tag because they are consumed at different points: steer and
	// next-turn at the save point, follow-up at the final output, and only
	// steer and follow-up may extend a run that was ending.
	steer    []TResponseInputItem
	nextTurn []TResponseInputItem
	followUp []TResponseInputItem
}

func newRunControl() *runControl { return &runControl{} }

func (c *runControl) StopAfterTurn() { c.stopAfterTurn.Store(true) }

func (c *runControl) stopRequested() bool { return c != nil && c.stopAfterTurn.Load() }

func (c *runControl) Phase() RunPhase { return RunPhase(c.phase.Load()) }

func (c *runControl) setPhase(p RunPhase) {
	if c != nil {
		c.phase.Store(int32(p))
	}
}

func (c *runControl) CurrentAgent() *Agent {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.currentAgent
}

func (c *runControl) CurrentTurn() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.currentTurn
}

func (c *runControl) setCurrent(agent *Agent, turn int) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.currentAgent = agent
	c.currentTurn = turn
	c.mu.Unlock()
}

// Steer implements RunControl.
func (c *runControl) Steer(input any) error { return c.enqueue(&c.steer, input) }

// NextTurn implements RunControl.
func (c *runControl) NextTurn(input any) error { return c.enqueue(&c.nextTurn, input) }

// FollowUp implements RunControl.
func (c *runControl) FollowUp(input any) error { return c.enqueue(&c.followUp, input) }

func (c *runControl) enqueue(q *[]TResponseInputItem, input any) error {
	items, err := normalizeInput(input)
	if err != nil {
		return err
	}
	if len(items) == 0 {
		return nil
	}
	c.mu.Lock()
	*q = append(*q, items...)
	c.mu.Unlock()
	return nil
}

// Pending implements RunControl.
func (c *runControl) Pending() PendingInput {
	c.mu.Lock()
	defer c.mu.Unlock()
	return PendingInput{
		Steer:    append([]TResponseInputItem(nil), c.steer...),
		NextTurn: append([]TResponseInputItem(nil), c.nextTurn...),
		FollowUp: append([]TResponseInputItem(nil), c.followUp...),
	}
}

// takeTurnInput drains what the save point delivers: steer first, then
// next-turn, so an urgent correction is read before a passenger message.
func (c *runControl) takeTurnInput() []TResponseInputItem {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.steer) == 0 && len(c.nextTurn) == 0 {
		return nil
	}
	out := make([]TResponseInputItem, 0, len(c.steer)+len(c.nextTurn))
	out = append(out, c.steer...)
	out = append(out, c.nextTurn...)
	c.steer, c.nextTurn = nil, nil
	return out
}

// takeContinuation drains what may extend a run that reached its final output:
// a steer that arrived too late for the save point, and any follow-up.
//
// next-turn is deliberately absent. It rides along with a turn the run was
// going to take anyway; making it force one would erase the only difference
// between it and Steer.
func (c *runControl) takeContinuation() []TResponseInputItem {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.steer) == 0 && len(c.followUp) == 0 {
		return nil
	}
	out := make([]TResponseInputItem, 0, len(c.steer)+len(c.followUp))
	out = append(out, c.steer...)
	out = append(out, c.followUp...)
	c.steer, c.followUp = nil, nil
	return out
}

// restore seeds the queues from a paused run's state, so input that arrived
// while a human was deciding on an approval is delivered when the run resumes
// rather than lost.
//
// A middleware that retries a resume re-enters this with the same control, so
// the seeding is per QUEUE STATE rather than once per control or once per
// attempt. Both extremes were wrong: appending every attempt delivered the
// human's steer to the model twice when the failed attempt had not consumed
// it, and seeding only once lost it entirely when the failed attempt had. A
// queue that still holds the previous attempt's copy is left alone; an empty
// one — which is what consuming leaves behind, since taking drains — is
// seeded again.
func (c *runControl) restore(p PendingInput) {
	if c == nil || p.Empty() {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.steer) == 0 {
		c.steer = append(c.steer, p.Steer...)
	}
	if len(c.nextTurn) == 0 {
		c.nextTurn = append(c.nextTurn, p.NextTurn...)
	}
	if len(c.followUp) == 0 {
		c.followUp = append(c.followUp, p.FollowUp...)
	}
}
