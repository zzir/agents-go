package agents

import (
	"context"
	"iter"
)

// streamOnlyModel adapts a streaming-only backend to the full Model contract.
type streamOnlyModel struct {
	inner Model
}

// NewStreamOnlyModel wraps a backend that rejects non-streaming requests so
// Respond is served by an internal StreamResponse call; the assembled response
// carries no RequestID — see decisions §5.15.
func NewStreamOnlyModel(inner Model) Model {
	return &streamOnlyModel{inner: inner}
}

func (m *streamOnlyModel) Respond(ctx context.Context, req ModelRequest) (*ModelResponse, error) {
	asm := &responseAssembler{}
	for event, err := range m.inner.StreamResponse(ctx, req) {
		if err != nil {
			return nil, err
		}
		if event == nil {
			continue
		}
		asm.observe(event)
	}
	return asm.result()
}

func (m *streamOnlyModel) StreamResponse(ctx context.Context, req ModelRequest) iter.Seq2[*ResponseStreamEvent, error] {
	return m.inner.StreamResponse(ctx, req)
}

var _ Model = (*streamOnlyModel)(nil)

// streamOnlyProvider wraps every Model from inner with NewStreamOnlyModel.
type streamOnlyProvider struct {
	inner ModelProvider
}

// NewStreamOnlyProvider is the provider-level NewStreamOnlyModel. Compose it
// innermost, next to the backend it adapts, so decorators above see ordinary
// Respond errors.
func NewStreamOnlyProvider(inner ModelProvider) ModelProvider {
	return &streamOnlyProvider{inner: inner}
}

func (p *streamOnlyProvider) Model(name string) (Model, error) {
	m, err := p.inner.Model(name)
	if err != nil {
		return nil, err
	}
	return NewStreamOnlyModel(m), nil
}
