package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	// the computed backoff. A hint longer than MaxDelay ends the retries with
	// that error rather than being clamped — see wait. Pair with
	// openai.RetryAfter.
	RetryAfter func(error) (time.Duration, bool)

	// Sleep waits for d or until ctx is done, returning ctx.Err() if cancelled.
	// When nil, a real timer is used. Tests inject a fake to avoid real waits.
	Sleep func(ctx context.Context, d time.Duration) error
}

// retryPolicyJSON is the JSON-friendly representation of RetryPolicy, using
// millisecond integer fields instead of time.Duration.
type retryPolicyJSON struct {
	MaxAttempts int     `json:"max_attempts"`
	BaseDelayMs int     `json:"base_delay_ms"`
	MaxDelayMs  int     `json:"max_delay_ms"`
	Multiplier  float64 `json:"multiplier"`
}

// UnmarshalJSON implements json.Unmarshaler. It accepts a JSON object with
// millisecond delay fields (base_delay_ms, max_delay_ms) and converts them to
// time.Duration, making RetryPolicy directly usable with json.Unmarshal from
// configuration stores.
func (p *RetryPolicy) UnmarshalJSON(data []byte) error {
	var raw retryPolicyJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	p.MaxAttempts = raw.MaxAttempts
	p.BaseDelay = time.Duration(raw.BaseDelayMs) * time.Millisecond
	p.MaxDelay = time.Duration(raw.MaxDelayMs) * time.Millisecond
	p.Multiplier = raw.Multiplier
	return nil
}

// MarshalJSON implements json.Marshaler, producing the millisecond-based JSON
// format that UnmarshalJSON consumes.
func (p RetryPolicy) MarshalJSON() ([]byte, error) {
	return json.Marshal(retryPolicyJSON{
		MaxAttempts: p.MaxAttempts,
		BaseDelayMs: int(p.BaseDelay / time.Millisecond),
		MaxDelayMs:  int(p.MaxDelay / time.Millisecond),
		Multiplier:  p.Multiplier,
	})
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

// maxDelay returns the effective backoff cap: MaxDelay, or 30s when unset.
func (p RetryPolicy) maxDelay() time.Duration {
	if p.MaxDelay <= 0 {
		return 30 * time.Second
	}
	return p.MaxDelay
}

// backoff returns the delay before the given attempt number (1-based; attempt 1
// is the first try and never waits, so callers pass attempt>=1 for the wait
// preceding attempt+1). Equal jitter keeps the delay in [d/2, d].
func (p RetryPolicy) backoff(attempt int) time.Duration {
	base := p.BaseDelay
	if base <= 0 {
		base = 500 * time.Millisecond
	}
	limit := p.maxDelay()
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
//
// A server-suggested delay longer than maxDelay **ends the retries** and
// returns that attempt's error, rather than being clamped down to the cap.
// Clamping would retry far sooner than the server said it would accept, which
// near-certainly fails again and burns the remaining attempts before reporting
// the same error anyway. Failing immediately says why, and says it faster. The
// error is wrapped, so errors.As and CodeOf still reach the original.
func (p RetryPolicy) wait(ctx context.Context, attempt int, err error) error {
	delay := p.backoff(attempt)
	if p.RetryAfter != nil {
		if d, ok := p.RetryAfter(err); ok {
			if limit := p.maxDelay(); d > limit {
				return fmt.Errorf("server asked to retry after %s, beyond the %s limit: %w",
					d.Round(time.Millisecond), limit, err)
			}
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
		// A run that answered after three retries looks identical to one that
		// answered first time. Record the difference, or nobody can explain the
		// latency afterwards.
		RecordDiagnostic(ctx, DiagModelRetry, err, map[string]any{
			"attempt": attempt, "max_attempts": maxAttempts, "streaming": false,
		})
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
				if producedAny {
					// A stream that broke after emitting cannot be retried —
					// the tokens are already out. Record it so a truncated
					// answer is explainable rather than merely odd.
					RecordDiagnostic(ctx, DiagStreamError, streamErr, map[string]any{"attempt": attempt})
				}
				yield(nil, streamErr)
				return
			}
			RecordDiagnostic(ctx, DiagModelRetry, streamErr, map[string]any{
				"attempt": attempt, "max_attempts": maxAttempts, "streaming": true,
			})
			if werr := m.policy.wait(ctx, attempt, streamErr); werr != nil {
				yield(nil, werr)
				return
			}
		}
	}
}

var _ Model = (*retryModel)(nil)

// retryProvider wraps a ModelProvider so every Model it produces retries per the
// given policy. This is the provider-level counterpart of NewRetryModel.
type retryProvider struct {
	inner  ModelProvider
	policy RetryPolicy
}

// NewRetryProvider wraps inner so that every Model it produces automatically
// retries per policy. It is the provider-level counterpart of NewRetryModel —
// use it when you know the retry policy at configuration time but not the model
// name.
func NewRetryProvider(inner ModelProvider, policy RetryPolicy) ModelProvider {
	return &retryProvider{inner: inner, policy: policy}
}

func (p *retryProvider) GetModel(name string) (Model, error) {
	m, err := p.inner.GetModel(name)
	if err != nil {
		return nil, err
	}
	return NewRetryModel(m, p.policy), nil
}
