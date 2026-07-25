package middleware

import (
	"context"
	"errors"
	"time"

	"github.com/zzir/agents-go/agents"
)

// Retry re-runs a run that failed.
//
// It is not the same as agents.NewRetryModel, and both are usually right at
// once. That one retries a single model call — a 429, a dropped connection —
// and the run never notices. This one retries the whole run, which is what a
// failure the loop could not absorb needs: a guardrail tripwire, a tool that
// exhausted its own retries, a max-turns overrun.
//
// A run that already produced items is retried from the start, not resumed.
// Resuming a failure means guessing which of its side effects happened, and
// the SDK cannot know: a tool may have written a file. Attach a Session if the
// history must survive the retry.
type Retry struct {
	// MaxAttempts includes the first. Zero means 2 — one retry.
	MaxAttempts int
	// Backoff waits before each retry. Nil waits not at all.
	Backoff func(attempt int) time.Duration
	// ShouldRetry decides from the error. Nil retries every failure except a
	// cancelled context, which is the caller saying stop.
	ShouldRetry func(err error) bool
}

// Run implements agents.RunMiddleware.
func (r Retry) Run(ctx context.Context, next agents.RunFunc, in agents.RunInput) agents.RunStream {
	attempts := r.MaxAttempts
	if attempts <= 0 {
		attempts = 2
	}
	should := r.ShouldRetry
	if should == nil {
		should = retriable
	}
	return func(yield func(agents.StreamEvent, error) bool) {
		for attempt := 1; ; attempt++ {
			res, live, err := collect(next(ctx, in), yield)
			if !live {
				return
			}
			if err == nil {
				finish(res, yield)
				return
			}
			if attempt >= attempts || !should(err) {
				yield(nil, err)
				return
			}
			if r.Backoff != nil {
				select {
				case <-ctx.Done():
					yield(nil, ctx.Err())
					return
				case <-time.After(r.Backoff(attempt)):
				}
			}
		}
	}
}

// retriable is the default predicate: retry anything except the caller
// deciding to stop.
func retriable(err error) bool {
	return !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}

var _ agents.RunMiddleware = Retry{}
