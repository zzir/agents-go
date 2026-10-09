package agents

import (
	"context"
	"slices"
)

// RunInput is what a middleware sees and may change before the run proceeds.
// Opts is the run's own copy, so edits apply to this run only.
type RunInput struct {
	Agent *Agent
	Input []InputItem
	Opts  *RunOptions
	// Control is the caller's handle on this run; a resuming middleware passes
	// it to ResumeRunWith — see spec §2.12.
	Control RunControl
}

// RunFunc executes a run and returns its stream; a middleware receives it as next.
type RunFunc func(ctx context.Context, in RunInput) RunStream

// RunMiddleware wraps a whole run. An implementation owes the three-clause
// stream contract (events flow through live; one RunCompletedEvent, last, on
// success only; nothing after the consumer stops) — see spec §2.12.
type RunMiddleware interface {
	Run(ctx context.Context, next RunFunc, in RunInput) RunStream
}

// RunMiddlewareFunc adapts a function to RunMiddleware.
type RunMiddlewareFunc func(ctx context.Context, next RunFunc, in RunInput) RunStream

// Run implements RunMiddleware.
func (f RunMiddlewareFunc) Run(ctx context.Context, next RunFunc, in RunInput) RunStream {
	return f(ctx, next, in)
}

// chainMiddleware wraps base so the first middleware in the slice is outermost.
func chainMiddleware(base RunFunc, mws []RunMiddleware) RunFunc {
	for _, mw := range slices.Backward(mws) {
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
