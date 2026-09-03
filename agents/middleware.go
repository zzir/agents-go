package agents

import (
	"context"
	"slices"
)

// RunInput is what a middleware sees and may change before the run proceeds.
// Opts is the run's own copy, by pointer, so a middleware can tighten a
// budget, swap a model or add a guardrail for this run and nothing else.
type RunInput struct {
	Agent *Agent
	Input []InputItem
	Opts  *RunOptions
	// Control is the handle the caller holds on this run. A middleware that
	// resumes a paused attempt hands it to ResumeRunWith, so the caller's stop
	// request and queued input survive the resume (spec §2.12).
	Control RunControl
}

// RunFunc executes a run and returns its stream. A middleware receives one as
// `next` and decides whether, when and with what to call it.
type RunFunc func(ctx context.Context, in RunInput) RunStream

// RunMiddleware wraps a run. It is the extension point for optional policy —
// retrying, logging, recovering, rewriting input — so those are not loop
// fields; what is deliberately NOT middleware is listed in spec §2.12.
//
// The stream contract (spec §2.12) — an implementation owes all three clauses:
//
//  1. Every event other than *RunCompletedEvent flows through as it happens.
//     Buffering until satisfied turns a live retry into an apparent hang.
//  2. *RunCompletedEvent appears exactly once, LAST, on a run that ends
//     without error — and zero times on one that errors. A middleware that
//     re-enters the run holds back each attempt's completion event and emits
//     a single one for the attempt it accepts.
//  3. Once the consumer stops ranging — yield returned false — nothing more
//     is yielded, not even an error. There is nobody to receive it.
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
