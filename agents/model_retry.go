package agents

import (
	"context"
	"errors"
	"iter"
	"math"
	"math/rand/v2"
	"time"
)

// RetryPolicy configures a RetryModel. The zero value is valid and uses the
// defaults documented on each field.
type RetryPolicy struct {
	// MaxAttempts is the total number of attempts, including the first. Values
	// <= 0 default to 3. Set to 1 to disable retrying.
	MaxAttempts int

	// BaseDelay is the backoff before the second attempt. Zero defaults to
	// 500ms. Subsequent delays grow geometrically by Multiplier, capped at
	// MaxDelay, with equal jitter applied.
	BaseDelay time.Duration
	// MaxDelay caps the backoff. Zero defaults to 30s.
	MaxDelay time.Duration
	// Multiplier is the geometric growth factor between attempts. Values <= 1
	// default to 2.
	Multiplier float64

	// RetryIf reports whether an error is worth retrying. When nil,
	// DefaultRetryIf is used (retry everything except context cancellation).
	// For OpenAI-aware classification (retry 429/5xx, not 4xx), pass
	// openai.RetryableError.
	RetryIf func(error) bool

	// RetryAfter, when non-nil, extracts a server-suggested delay from an error
	// (e.g. an HTTP Retry-After header); when it reports ok, that delay replaces
	// the computed backoff. Pair with openai.RetryAfter.
	RetryAfter func(error) (time.Duration, bool)

	// Sleep waits for d or until ctx is done, returning ctx.Err() if cancelled.
	// When nil, a real timer is used. Tests inject a fake to avoid real waits.
	Sleep func(ctx context.Context, d time.Duration) error
}

// DefaultRetryIf retries every error except context cancellation or deadline
// expiry (retrying those is pointless once the caller's context is done).
func DefaultRetryIf(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	return true
}

func (p RetryPolicy) maxAttempts() int {
	if p.MaxAttempts <= 0 {
		return 3
	}
	return p.MaxAttempts
}

func (p RetryPolicy) retryIf() func(error) bool {
	if p.RetryIf != nil {
		return p.RetryIf
	}
	return DefaultRetryIf
}

// backoff returns the delay before the given attempt number (1-based; attempt 1
// is the first try and never waits, so callers pass attempt>=1 for the wait
// preceding attempt+1). Equal jitter keeps the delay in [d/2, d].
func (p RetryPolicy) backoff(attempt int) time.Duration {
	base := p.BaseDelay
	if base <= 0 {
		base = 500 * time.Millisecond
	}
	limit := p.MaxDelay
	if limit <= 0 {
		limit = 30 * time.Second
	}
	mult := p.Multiplier
	if mult <= 1 {
		mult = 2
	}
	d := float64(base) * math.Pow(mult, float64(attempt-1))
	if d > float64(limit) {
		d = float64(limit)
	}
	half := d / 2
	return time.Duration(half + rand.Float64()*half)
}

// wait sleeps for the backoff (or server-suggested) delay before the next
// attempt. attempt is the number of the attempt that just failed.
func (p RetryPolicy) wait(ctx context.Context, attempt int, err error) error {
	delay := p.backoff(attempt)
	if p.RetryAfter != nil {
		if d, ok := p.RetryAfter(err); ok {
			delay = d
		}
	}
	if p.Sleep != nil {
		return p.Sleep(ctx, delay)
	}
	return sleepCtx(ctx, delay)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// retryModel wraps a Model and retries transient failures with backoff.
type retryModel struct {
	inner  Model
	policy RetryPolicy
}

// NewRetryModel wraps inner so that failing GetResponse calls (and StreamResponse
// calls that fail before emitting any event) are retried per policy. It is a
// provider-agnostic Model decorator; compose it with NewFallbackModel.
func NewRetryModel(inner Model, policy RetryPolicy) Model {
	return &retryModel{inner: inner, policy: policy}
}

func (m *retryModel) GetResponse(ctx context.Context, req ModelRequest) (*ModelResponse, error) {
	retryIf := m.policy.retryIf()
	maxAttempts := m.policy.maxAttempts()
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		resp, err := m.inner.GetResponse(ctx, req)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if attempt == maxAttempts || !retryIf(err) {
			break
		}
		if werr := m.policy.wait(ctx, attempt, err); werr != nil {
			return nil, werr
		}
	}
	return nil, lastErr
}

// StreamResponse retries only when the inner stream fails before yielding any
// event. Once a token has been emitted it cannot be un-sent, so a later error is
// passed straight through.
func (m *retryModel) StreamResponse(ctx context.Context, req ModelRequest) iter.Seq2[*TResponseStreamEvent, error] {
	return func(yield func(*TResponseStreamEvent, error) bool) {
		retryIf := m.policy.retryIf()
		maxAttempts := m.policy.maxAttempts()
		for attempt := 1; attempt <= maxAttempts; attempt++ {
			producedAny := false
			var streamErr error
			for ev, err := range m.inner.StreamResponse(ctx, req) {
				if err != nil {
					streamErr = err
					break
				}
				producedAny = true
				if !yield(ev, nil) {
					return
				}
			}
			if streamErr == nil {
				return
			}
			if producedAny || attempt == maxAttempts || !retryIf(streamErr) {
				yield(nil, streamErr)
				return
			}
			if werr := m.policy.wait(ctx, attempt, streamErr); werr != nil {
				yield(nil, werr)
				return
			}
		}
	}
}

var _ Model = (*retryModel)(nil)
