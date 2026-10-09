package tracing

import "context"

type spanKey struct{}

// WithSpan returns a context carrying span as the current parent: the one channel
// a Model decorator, MCP client or sandbox backend receives — see spec §2.11e.
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
// span and a context parented to it; without a trace the handle is a usable
// no-op — see spec §2.11e.
func StartSpanFrom(ctx context.Context, name, spanType string, data map[string]any) (*SpanHandle, context.Context) {
	parent := SpanFrom(ctx)
	if parent == nil {
		return &SpanHandle{}, ctx
	}
	sp := parent.StartTypedSpan(name, spanType, data)
	return sp, WithSpan(ctx, sp)
}
