package agents

import (
	"context"
	"errors"
	"fmt"
	"iter"

	"github.com/zzir/agents-go/tracing"
)

// FallbackModel tries a chain of Models in order until one succeeds.
type FallbackModel struct {
	models         []Model
	shouldFallback func(error) bool
}

// NewFallbackModel returns a Model that tries primary, then each fallback in
// order, until one succeeds (DefaultRetryIf decides); all failing returns the
// joined error. Wrap each backend in NewRetryModel first:
//
//	NewFallbackModel(NewRetryModel(primary, p), NewRetryModel(backup, p))
func NewFallbackModel(primary Model, fallbacks ...Model) *FallbackModel {
	models := make([]Model, 0, len(fallbacks)+1)
	models = append(models, primary)
	models = append(models, fallbacks...)
	return &FallbackModel{models: models, shouldFallback: DefaultRetryIf}
}

// WithShouldFallback replaces the classifier deciding whether a failure moves
// the chain on (default DefaultRetryIf; openai.RetryableError fails 4xx fast).
// nil keeps the current one; returns m for chaining.
func (m *FallbackModel) WithShouldFallback(f func(error) bool) *FallbackModel {
	if f != nil {
		m.shouldFallback = f
	}
	return m
}

// Respond implements Model: backends in order until one succeeds or the
// classifier stops the chain; all errors are joined.
func (m *FallbackModel) Respond(ctx context.Context, req ModelRequest) (*ModelResponse, error) {
	var errs []error
	for i, inner := range m.models {
		resp, err := inner.Respond(ctx, req)
		if err == nil {
			if i > 0 {
				// Record the fallback so a primary outage is visible.
				RecordDiagnostic(ctx, DiagModelFallback, errors.Join(errs...), map[string]any{
					"used_index": i, "models": len(m.models), "streaming": false,
				})
				tracing.SpanFrom(ctx).Set("fallback_index", i)
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

// StreamResponse implements Model: a backend can be swapped only before its
// first output event — see decisions §5.16.
func (m *FallbackModel) StreamResponse(ctx context.Context, req ModelRequest) iter.Seq2[*ResponseStreamEvent, error] {
	return func(yield func(*ResponseStreamEvent, error) bool) {
		var errs []error
		for i, inner := range m.models {
			a := deliverStreamAttempt(inner.StreamResponse(ctx, req), yield, nil)
			if a.stopped {
				return
			}
			if a.err == nil {
				// Clean finish: deliver the held-back events.
				if !flushStreamEvents(a.pending, yield) {
					return
				}
				if i > 0 {
					RecordDiagnostic(ctx, DiagModelFallback, errors.Join(errs...), map[string]any{
						"used_index": i, "models": len(m.models), "streaming": true,
					})
					tracing.SpanFrom(ctx).Set("fallback_index", i)
				}
				return
			}
			errs = append(errs, a.err)
			if a.committed || i == len(m.models)-1 || !m.shouldFallback(a.err) {
				if a.committed {
					// A committed backend cannot be swapped; record which one.
					RecordDiagnostic(ctx, DiagStreamError, a.err, map[string]any{
						"used_index": i, "models": len(m.models),
					})
				} else if !flushStreamEvents(a.pending, yield) {
					// No further backend: flush the held-back events ahead of
					// the error.
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

// NewFallbackProvider is the provider-level NewFallbackModel: every Model it
// produces falls back through each fallback provider's model.
func NewFallbackProvider(primary ModelProvider, fallbacks ...ModelProvider) *FallbackProvider {
	return &FallbackProvider{primary: primary, fallbacks: fallbacks}
}

// WithShouldFallback sets the classifier for every FallbackModel this provider
// produces, as (*FallbackModel).WithShouldFallback; returns p for chaining.
func (p *FallbackProvider) WithShouldFallback(f func(error) bool) *FallbackProvider {
	if f != nil {
		p.shouldFallback = f
	}
	return p
}

// Model implements ModelProvider: a FallbackModel chaining the fallbacks that
// resolve name; every configured fallback failing to resolve is an error, and
// no fallbacks configured returns the primary unchanged.
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
