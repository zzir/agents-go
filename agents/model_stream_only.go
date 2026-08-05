package agents

import (
	"context"
	"iter"
)

// streamOnlyModel adapts a streaming-only backend to the full Model contract.
type streamOnlyModel struct {
	inner Model
}

// NewStreamOnlyModel wraps inner so Respond is served by an internal
// StreamResponse call, assembled into a ModelResponse from the terminal
// event. Use it for backends that reject non-streaming requests outright —
// the ChatGPT Codex backend, for example, accepts only "stream": true — so
// blocking callers (RunSync, one-shot summarization calls) still work.
// StreamResponse passes through untouched.
//
// Like the runner's own streaming path, the assembled response carries no
// RequestID: the transport header never reaches the event stream.
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

// NewStreamOnlyProvider wraps inner so every Model it produces serves
// Respond via an internal stream — the provider-level counterpart of
// NewStreamOnlyModel. Compose it innermost, next to the backend it adapts:
// decorators above it (retry, fallback, routing) then see blocking calls
// fail as ordinary Respond errors and handle them normally.
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
