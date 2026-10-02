package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"math"
	"math/rand/v2"
	"net"
	"time"

	"github.com/zzir/agents-go/tracing"
)

// ErrAttemptTimeout and ErrIdleTimeout are the errors of an attempt the retry
// layer's own clocks ended (RetryPolicy.AttemptTimeout / IdleTimeout), wrapped
// in what the attempt returns — so a caller tells them from its own deadline.
var (
	ErrAttemptTimeout = errors.New("agents: model attempt timed out")
	ErrIdleTimeout    = errors.New("agents: model stream idle timed out")
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

	// AttemptTimeout bounds one attempt apart from the caller's ctx: a
	// blocking call wholly, a streaming call until its first output event.
	// Zero is no bound. An attempt it ends is retried; the error wraps
	// ErrAttemptTimeout — see spec §2.16.
	AttemptTimeout time.Duration
	// IdleTimeout bounds the silence between two events of a streaming
	// attempt, the first included. Zero is no bound. Before output the
	// attempt is retried; after it the stream ends with ErrIdleTimeout.
	IdleTimeout time.Duration

	// sleep waits for d or until ctx is done, returning ctx.Err() if cancelled.
	// When nil, a real timer is used. Tests inject a fake to avoid real waits.
	sleep func(ctx context.Context, d time.Duration) error
}

// retryPolicyJSON is the JSON-friendly representation of RetryPolicy, using
// millisecond integer fields instead of time.Duration.
type retryPolicyJSON struct {
	MaxAttempts      int     `json:"max_attempts"`
	BaseDelayMs      int     `json:"base_delay_ms"`
	MaxDelayMs       int     `json:"max_delay_ms"`
	Multiplier       float64 `json:"multiplier"`
	AttemptTimeoutMs int     `json:"attempt_timeout_ms,omitempty"`
	IdleTimeoutMs    int     `json:"idle_timeout_ms,omitempty"`
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
	p.AttemptTimeout = time.Duration(raw.AttemptTimeoutMs) * time.Millisecond
	p.IdleTimeout = time.Duration(raw.IdleTimeoutMs) * time.Millisecond
	return nil
}

// MarshalJSON implements json.Marshaler, producing the millisecond-based JSON
// format that UnmarshalJSON consumes.
func (p RetryPolicy) MarshalJSON() ([]byte, error) {
	return json.Marshal(retryPolicyJSON{
		MaxAttempts:      p.MaxAttempts,
		BaseDelayMs:      int(p.BaseDelay / time.Millisecond),
		MaxDelayMs:       int(p.MaxDelay / time.Millisecond),
		Multiplier:       p.Multiplier,
		AttemptTimeoutMs: int(p.AttemptTimeout / time.Millisecond),
		IdleTimeoutMs:    int(p.IdleTimeout / time.Millisecond),
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

// backoff returns the delay after attempt (1-based, the try that just finished).
// Equal jitter keeps the delay in [d/2, d].
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

// wait sleeps the backoff (or server-suggested) delay after attempt, the try that
// just failed; a suggestion past maxDelay ends the retries, returning err wrapped.
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
	if p.sleep != nil {
		return p.sleep(ctx, delay)
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

// attemptWatch is the policy's clock on one attempt: it cancels the attempt's
// context, with the matching error as cause, when AttemptTimeout or (streaming)
// IdleTimeout runs out. Zero durations arm nothing.
type attemptWatch struct {
	ctx    context.Context
	cancel context.CancelCauseFunc
	total  *time.Timer
	idle   *time.Timer
	idleD  time.Duration
}

func (p RetryPolicy) watch(parent context.Context, streaming bool) *attemptWatch {
	w := &attemptWatch{}
	w.ctx, w.cancel = context.WithCancelCause(parent)
	if p.AttemptTimeout > 0 {
		w.total = time.AfterFunc(p.AttemptTimeout, func() { w.cancel(ErrAttemptTimeout) })
	}
	if streaming && p.IdleTimeout > 0 {
		w.idleD = p.IdleTimeout
		w.idle = time.AfterFunc(p.IdleTimeout, func() { w.cancel(ErrIdleTimeout) })
	}
	return w
}

// event notes a stream event: the silence clock restarts, and the first output
// event releases the attempt clock (a committed stream may run long).
func (w *attemptWatch) event(committed bool) {
	if w.idle != nil {
		w.idle.Reset(w.idleD)
	}
	if committed && w.total != nil {
		w.total.Stop()
		w.total = nil
	}
}

// end releases the clocks and reports which of them, if any, ended the attempt.
func (w *attemptWatch) end() error {
	if w.total != nil {
		w.total.Stop()
	}
	if w.idle != nil {
		w.idle.Stop()
	}
	cause := context.Cause(w.ctx)
	w.cancel(nil)
	if errors.Is(cause, ErrAttemptTimeout) || errors.Is(cause, ErrIdleTimeout) {
		return cause
	}
	return nil
}

// timeoutError is the attempt's error when a clock ended it: the inner error
// only says cancelled, which would read as the caller's own stop.
func (p RetryPolicy) timeoutError(cause error) error {
	d := p.AttemptTimeout
	if errors.Is(cause, ErrIdleTimeout) {
		d = p.IdleTimeout
	}
	return fmt.Errorf("%w after %s", cause, d)
}

// stateful reports a request that chains server-side state, where a replayed
// attempt could repeat the turn — see spec §2.16.
func (req ModelRequest) stateful() bool {
	return req.PreviousResponseID != "" || req.ConversationID != ""
}

// replaySafe reports a failure known not to have applied the request: the
// server answered it (any error that is not transport-shaped), or the dial
// itself failed. A deadline or a connection severed after the send is ambiguous.
func replaySafe(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return false
	}
	if op, ok := errors.AsType[*net.OpError](err); ok {
		return op.Op == "dial"
	}
	_, isNet := errors.AsType[net.Error](err)
	return !isNet
}

// retryable decides whether a failed attempt may be replaced: one the policy's
// clock ended is, unless the request is stateful; otherwise RetryIf decides,
// and a stateful request also needs the failure to be replay-safe.
func (p RetryPolicy) retryable(req ModelRequest, err error, timedOut bool) bool {
	if req.stateful() && (timedOut || !replaySafe(err)) {
		return false
	}
	return timedOut || p.retryIf()(err)
}

// retryModel wraps a Model and retries transient failures with backoff.
type retryModel struct {
	inner  Model
	policy RetryPolicy
}

// NewRetryModel wraps inner so that failing Respond calls (and
// StreamResponse calls that fail before any output event) are retried per
// policy. It is a provider-agnostic Model decorator; compose it with
// NewFallbackModel.
func NewRetryModel(inner Model, policy RetryPolicy) Model {
	return &retryModel{inner: inner, policy: policy}
}

func (m *retryModel) Respond(ctx context.Context, req ModelRequest) (*ModelResponse, error) {
	maxAttempts := m.policy.maxAttempts()
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		w := m.policy.watch(ctx, false)
		resp, err := m.inner.Respond(w.ctx, req)
		timedOut := w.end()
		if err == nil {
			return resp, nil
		}
		if timedOut != nil {
			err = m.policy.timeoutError(timedOut)
		}
		lastErr = err
		if attempt == maxAttempts || !m.policy.retryable(req, err, timedOut != nil) {
			break
		}
		// Record the retry so the extra latency is explainable afterward.
		RecordDiagnostic(ctx, DiagModelRetry, err, map[string]any{
			"attempt": attempt, "max_attempts": maxAttempts, "streaming": false,
		})
		retrySpan(ctx, attempt, maxAttempts, false, err)
		if werr := m.policy.wait(ctx, attempt, err); werr != nil {
			return nil, werr
		}
	}
	return nil, lastErr
}

// StreamResponse retries only while the inner stream has yielded no output:
// pre-commit events are held back until the first output event commits the
// attempt (deliverStreamAttempt), so a stream that dies early is retried like a
// failed connection. Once output is emitted the attempt is committed and a
// later error passes straight through. See decisions §5.16.
func (m *retryModel) StreamResponse(ctx context.Context, req ModelRequest) iter.Seq2[*ResponseStreamEvent, error] {
	return func(yield func(*ResponseStreamEvent, error) bool) {
		maxAttempts := m.policy.maxAttempts()
		for attempt := 1; attempt <= maxAttempts; attempt++ {
			w := m.policy.watch(ctx, true)
			a := deliverStreamAttempt(m.inner.StreamResponse(w.ctx, req), yield, w.event)
			timedOut := w.end()
			if a.stopped {
				return
			}
			if a.err == nil {
				// Clean finish: deliver held-back events (an all-pending stream
				// still delivers rather than vanishing). Nothing follows, so the
				// flush bool needs no check here.
				flushStreamEvents(a.pending, yield)
				return
			}
			if timedOut != nil {
				a.err = m.policy.timeoutError(timedOut)
			}
			if a.committed || attempt == maxAttempts || !m.policy.retryable(req, a.err, timedOut != nil) {
				if a.committed {
					// A committed stream cannot be retried; record the break so a
					// truncated answer is explainable.
					RecordDiagnostic(ctx, DiagStreamError, a.err, map[string]any{"attempt": attempt})
				} else if !flushStreamEvents(a.pending, yield) {
					// No further attempt: flush the held-back events ahead of the error.
					return
				}
				yield(nil, a.err)
				return
			}
			RecordDiagnostic(ctx, DiagModelRetry, a.err, map[string]any{
				"attempt": attempt, "max_attempts": maxAttempts, "streaming": true,
			})
			retrySpan(ctx, attempt, maxAttempts, true, a.err)
			if werr := m.policy.wait(ctx, attempt, a.err); werr != nil {
				yield(nil, werr)
				return
			}
			// a.pending is dropped: the next attempt opens its own response.
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

func (p *retryProvider) Model(name string) (Model, error) {
	m, err := p.inner.Model(name)
	if err != nil {
		return nil, err
	}
	return NewRetryModel(m, p.policy), nil
}

// retrySpan records one failed attempt as a zero-duration span under the
// generation span it belongs to.
func retrySpan(ctx context.Context, attempt, maxAttempts int, streaming bool, err error) {
	sp, _ := tracing.StartSpanFrom(ctx, "model_retry", tracing.SpanTypeModelRetry, map[string]any{
		"attempt": attempt, "max_attempts": maxAttempts, "streaming": streaming,
	})
	sp.SetError(err.Error(), nil)
	sp.Finish()
}
