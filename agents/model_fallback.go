package agents

import (
	"context"
	"errors"
	"iter"
)

// fallbackModel tries a chain of Models in order until one succeeds.
type fallbackModel struct {
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
func NewFallbackModel(primary Model, fallbacks ...Model) Model {
	models := make([]Model, 0, len(fallbacks)+1)
	models = append(models, primary)
	models = append(models, fallbacks...)
	return &fallbackModel{models: models, shouldFallback: DefaultRetryIf}
}

func (m *fallbackModel) GetResponse(ctx context.Context, req ModelRequest) (*ModelResponse, error) {
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

func (m *fallbackModel) StreamResponse(ctx context.Context, req ModelRequest) iter.Seq2[*TResponseStreamEvent, error] {
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

var _ Model = (*fallbackModel)(nil)
