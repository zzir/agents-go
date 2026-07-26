package tracing

import "context"

type spanKey struct{}

// WithSpan returns a context carrying span as the current parent.
//
// It is how a subsystem the runner does not own — an MCP client, a sandbox
// backend, a Model decorator — nests its work under the run that triggered it.
// Those receive a context.Context and nothing else belonging to the run, so the
// context is the only channel; threading a span handle through every signature
// would mean every implementation had to forward it, and the one that forgot
// would silently orphan its spans at the root.
func WithSpan(ctx context.Context, span *SpanHandle) context.Context {
	if span == nil || span.Span == nil {
		return ctx
	}
	return context.WithValue(ctx, spanKey{}, span)
}

// SpanFrom returns the current parent span on ctx, or nil.
func SpanFrom(ctx context.Context) *SpanHandle {
	h, _ := ctx.Value(spanKey{}).(*SpanHandle)
	return h
}

// StartSpanFrom begins a typed span under whatever ctx carries, returning the
// span and a context parented to it.
//
// It returns a usable no-op handle when there is no trace, so an instrumented
// call site never needs a branch — and a subsystem used outside a run behaves
// exactly as it did before it was instrumented.
func StartSpanFrom(ctx context.Context, name, spanType string, data map[string]any) (*SpanHandle, context.Context) {
	parent := SpanFrom(ctx)
	if parent == nil {
		return &SpanHandle{}, ctx
	}
	sp := parent.StartTypedSpan(name, spanType, data)
	return sp, WithSpan(ctx, sp)
}
