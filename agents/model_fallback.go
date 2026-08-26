package agents

import (
	"context"
	"errors"
	"fmt"
	"iter"
)

// FallbackModel tries a chain of Models in order until one succeeds.
type FallbackModel struct {
	models         []Model
	shouldFallback func(error) bool
}

// NewFallbackModel returns a Model that tries primary first and, on failure,
// falls back to each model in fallbacks in order. The first success is returned;
// if all fail, the joined error is returned. Fallback stops early on context
// cancellation (see DefaultRetryIf).
//
// Wrap each backend in NewRetryModel first so every provider exhausts its own
// retries before the chain moves on:
//
//	NewFallbackModel(NewRetryModel(primary, p), NewRetryModel(backup, p))
//
// For streaming, a model can only be skipped if it failed before emitting any
// output event; once a token is produced the chain commits to that model.
func NewFallbackModel(primary Model, fallbacks ...Model) *FallbackModel {
	models := make([]Model, 0, len(fallbacks)+1)
	models = append(models, primary)
	models = append(models, fallbacks...)
	return &FallbackModel{models: models, shouldFallback: DefaultRetryIf}
}

// WithShouldFallback replaces the error classifier that decides whether the
// chain moves on to the next backend after a failure. The default
// (DefaultRetryIf) falls back on every error except context cancellation; pass
// e.g. openai.RetryableError so deterministic client errors (4xx) fail fast
// instead of being retried against every backend. A nil f keeps the current
// classifier. It returns m for chaining.
func (m *FallbackModel) WithShouldFallback(f func(error) bool) *FallbackModel {
	if f != nil {
		m.shouldFallback = f
	}
	return m
}

// Respond implements Model: backends are tried in order until one
// succeeds or the classifier stops the chain; all errors are joined.
func (m *FallbackModel) Respond(ctx context.Context, req ModelRequest) (*ModelResponse, error) {
	var errs []error
	for i, inner := range m.models {
		resp, err := inner.Respond(ctx, req)
		if err == nil {
			if i > 0 {
				// Record the fallback, or a primary outage shows only as slower answers.
				RecordDiagnostic(ctx, DiagModelFallback, errors.Join(errs...), map[string]any{
					"used_index": i, "models": len(m.models), "streaming": false,
				})
			}
			return resp, nil
		}
		errs = append(errs, err)
		if !m.shouldFallback(err) {
			break
		}
	}
	return nil, errors.Join(errs...)
}

// StreamResponse implements Model: a backend can only be swapped before its
// first output event; once output commits the backend, a mid-stream error is
// surfaced as-is and recorded as DiagStreamError. See decisions §5.16.
func (m *FallbackModel) StreamResponse(ctx context.Context, req ModelRequest) iter.Seq2[*ResponseStreamEvent, error] {
	return func(yield func(*ResponseStreamEvent, error) bool) {
		var errs []error
		for i, inner := range m.models {
			a := deliverStreamAttempt(inner.StreamResponse(ctx, req), yield)
			if a.stopped {
				return
			}
			if a.err == nil {
				// Clean finish: deliver held-back events (an all-pending stream
				// still delivers rather than vanishing).
				if !flushStreamEvents(a.pending, yield) {
					return
				}
				if i > 0 {
					RecordDiagnostic(ctx, DiagModelFallback, errors.Join(errs...), map[string]any{
						"used_index": i, "models": len(m.models), "streaming": true,
					})
				}
				return
			}
			errs = append(errs, a.err)
			if a.committed || i == len(m.models)-1 || !m.shouldFallback(a.err) {
				if a.committed {
					// A committed backend cannot be swapped; record which one, so
					// a truncated answer is explainable (decisions §5.16).
					RecordDiagnostic(ctx, DiagStreamError, a.err, map[string]any{
						"used_index": i, "models": len(m.models),
					})
				} else if !flushStreamEvents(a.pending, yield) {
					// No further backend: flush the held-back events ahead of the error.
					return
				}
				yield(nil, errors.Join(errs...))
				return
			}
			// a.pending is dropped: the next backend opens its own response.
		}
	}
}

var _ Model = (*FallbackModel)(nil)

// FallbackProvider wraps a primary ModelProvider with fallback alternatives.
// Each Model call produces a FallbackModel chaining every provider's model.
type FallbackProvider struct {
	primary        ModelProvider
	fallbacks      []ModelProvider
	shouldFallback func(error) bool
}

// NewFallbackProvider wraps primary so that every Model it produces automatically
// falls back through the models from each fallback provider. It is the
// provider-level counterpart of NewFallbackModel.
func NewFallbackProvider(primary ModelProvider, fallbacks ...ModelProvider) *FallbackProvider {
	return &FallbackProvider{primary: primary, fallbacks: fallbacks}
}

// WithShouldFallback sets the error classifier applied to every FallbackModel
// this provider produces, with the same semantics as
// (*FallbackModel).WithShouldFallback. A nil f keeps the default. It returns p
// for chaining.
func (p *FallbackProvider) WithShouldFallback(f func(error) bool) *FallbackProvider {
	if f != nil {
		p.shouldFallback = f
	}
	return p
}

// Model implements ModelProvider: it resolves name on the primary and each
// fallback provider, returning a FallbackModel chaining the results (with this
// provider's classifier applied).
//
// If some fallbacks resolve and others do not, the working (shorter) chain is
// returned. If fallbacks were configured but every one fails to resolve, the
// aggregated error is returned rather than silently degrading to a bare
// primary. With no fallbacks configured at all, the primary is returned
// unchanged.
func (p *FallbackProvider) Model(name string) (Model, error) {
	m, err := p.primary.Model(name)
	if err != nil {
		return nil, err
	}
	var (
		fbs  []Model
		errs []error
	)
	for _, fp := range p.fallbacks {
		fm, ferr := fp.Model(name)
		if ferr != nil {
			errs = append(errs, ferr)
			continue
		}
		fbs = append(fbs, fm)
	}
	if len(fbs) == 0 {
		if len(errs) > 0 {
			return nil, fmt.Errorf("fallback provider: all %d configured fallback provider(s) failed to resolve model %q: %w",
				len(errs), name, errors.Join(errs...))
		}
		return m, nil
	}
	return NewFallbackModel(m, fbs...).WithShouldFallback(p.shouldFallback), nil
}

var _ ModelProvider = (*FallbackProvider)(nil)
