package agents

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"log/slog"
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
// event; once a token is produced the chain commits to that model.
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

// GetResponse implements Model: backends are tried in order until one
// succeeds or the classifier stops the chain; all errors are joined.
func (m *FallbackModel) GetResponse(ctx context.Context, req ModelRequest) (*ModelResponse, error) {
	var errs []error
	for _, inner := range m.models {
		resp, err := inner.GetResponse(ctx, req)
		if err == nil {
			return resp, nil
		}
		errs = append(errs, err)
		if !m.shouldFallback(err) {
			break
		}
	}
	return nil, errors.Join(errs...)
}

// StreamResponse implements Model. A backend can only be swapped before its
// first event is emitted; after that, a mid-stream error is surfaced as-is.
func (m *FallbackModel) StreamResponse(ctx context.Context, req ModelRequest) iter.Seq2[*TResponseStreamEvent, error] {
	return func(yield func(*TResponseStreamEvent, error) bool) {
		var errs []error
		for i, inner := range m.models {
			producedAny := false
			var streamErr error
			for ev, err := range inner.StreamResponse(ctx, req) {
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
			errs = append(errs, streamErr)
			if producedAny || i == len(m.models)-1 || !m.shouldFallback(streamErr) {
				yield(nil, errors.Join(errs...))
				return
			}
		}
	}
}

var _ Model = (*FallbackModel)(nil)

// FallbackProvider wraps a primary ModelProvider with fallback alternatives.
// Each GetModel call produces a FallbackModel chaining every provider's model.
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

// GetModel implements ModelProvider: it resolves name on the primary and each
// fallback provider, returning a FallbackModel chaining the results (with this
// provider's classifier applied).
//
// Fallback resolution errors are not discarded. If some fallbacks resolve and
// others do not, the working chain is returned and the failures are logged. If
// fallbacks were configured but every one fails to resolve, the aggregated
// error is returned rather than silently degrading to a bare primary — which
// would disable the fallback protection the caller asked for. With no fallbacks
// configured at all, the primary is returned unchanged.
func (p *FallbackProvider) GetModel(name string) (Model, error) {
	m, err := p.primary.GetModel(name)
	if err != nil {
		return nil, err
	}
	var (
		fbs  []Model
		errs []error
	)
	for _, fp := range p.fallbacks {
		fm, ferr := fp.GetModel(name)
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
	if len(errs) > 0 {
		// At least one fallback still resolved, so keep the (shorter) chain, but
		// surface the partial failure — a silently shrunk fallback set otherwise
		// looks healthy until the moment it is needed.
		slog.Warn("fallback provider: some fallback providers failed to resolve model; continuing with the rest",
			"model", name, "failed", len(errs), "resolved", len(fbs), "error", errors.Join(errs...))
	}
	return NewFallbackModel(m, fbs...).WithShouldFallback(p.shouldFallback), nil
}

var _ ModelProvider = (*FallbackProvider)(nil)
