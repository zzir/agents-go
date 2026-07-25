package agents

import "context"

// RunInput is what a middleware sees and may change before the run proceeds.
//
// Opts is a pointer so a middleware can adjust the run's configuration —
// tighten a budget, swap a model, add a guardrail — without rebuilding it. It
// is the run's own copy, so an edit affects this run and nothing else.
type RunInput struct {
	Agent *Agent
	Input []TResponseInputItem
	Opts  *RunOptions
}

// RunFunc executes a run and returns its stream. A middleware receives one as
// `next` and decides whether, when and with what to call it.
type RunFunc func(ctx context.Context, in RunInput) RunStream

// RunMiddleware wraps a run.
//
// It is the extension point for **optional policy** — retrying, logging,
// recovering from an error, rewriting input — so those stop being fields on
// RunOptions that the loop has to check for. A middleware that only observes
// calls next and passes the stream through; one that intervenes can inspect
// events, replace them, run the loop twice, or not at all.
//
// What is deliberately NOT middleware: handoffs, guardrails, session
// persistence and tracing. Those are not policies layered over the loop, they
// are the loop — a handoff changes which agent the state machine is in,
// guardrails race the model call and cancel it, persistence has a boundary that
// only the loop knows. Expressing them as middleware would turn three
// invariants into implicit protocols between wrappers.
type RunMiddleware interface {
	Run(ctx context.Context, next RunFunc, in RunInput) RunStream
}

// RunMiddlewareFunc adapts a function to RunMiddleware.
type RunMiddlewareFunc func(ctx context.Context, next RunFunc, in RunInput) RunStream

// Run implements RunMiddleware.
func (f RunMiddlewareFunc) Run(ctx context.Context, next RunFunc, in RunInput) RunStream {
	return f(ctx, next, in)
}

// chainMiddleware wraps base so the first middleware in the slice is the
// outermost — the order they are read in is the order they see the run.
func chainMiddleware(base RunFunc, mws []RunMiddleware) RunFunc {
	for i := len(mws) - 1; i >= 0; i-- {
		mw := mws[i]
		if mw == nil {
			continue
		}
		next := base
		base = func(ctx context.Context, in RunInput) RunStream {
			return mw.Run(ctx, next, in)
		}
	}
	return base
}
