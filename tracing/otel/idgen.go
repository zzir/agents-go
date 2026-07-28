package otel

import (
	"context"
	"crypto/rand"
	"sync"

	oteltrace "go.opentelemetry.io/otel/trace"
)

// IDGenerator hands the OTel SDK the exact ids a span already has, instead of
// minting new ones.
//
// This is what makes after-the-fact reconstruction work at all. The naive
// approach — inject a remote parent SpanContext and let the SDK generate ids —
// links children correctly but gives each root span a **freshly generated trace
// id**, splitting one trace into several. Pinning both ids keeps the whole tree
// under the trace id the run actually used.
//
// It is stateful by design: Exporter.emit sets the pin, then starts the span,
// and the SDK reads the pin during Start. Exporter serializes that pair under a
// mutex; a caller sharing one generator across concurrent exporters would get
// crossed ids.
//
// When nothing is pinned it falls back to random ids. Do NOT share the
// TracerProvider with ordinary application instrumentation: only the Exporter
// serializes pin-then-Start, so an app span started between another span's pin
// and Start would CONSUME the pin — emitted under the agent span's exact ids
// while the agent span is orphaned onto random ones. Dedicate the provider to
// the exporter.
type IDGenerator struct {
	mu      sync.Mutex
	traceID oteltrace.TraceID
	spanID  oteltrace.SpanID
	pinned  bool
}

// NewIDGenerator returns a generator with nothing pinned.
func NewIDGenerator() *IDGenerator { return &IDGenerator{} }

// pin sets the ids the next NewIDs / NewSpanID call will return. It is
// consumed by that call.
func (g *IDGenerator) pin(traceID oteltrace.TraceID, spanID oteltrace.SpanID) {
	g.mu.Lock()
	g.traceID, g.spanID, g.pinned = traceID, spanID, true
	g.mu.Unlock()
}

// NewIDs implements sdktrace.IDGenerator for a root span.
func (g *IDGenerator) NewIDs(context.Context) (oteltrace.TraceID, oteltrace.SpanID) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.pinned {
		g.pinned = false
		return g.traceID, g.spanID
	}
	return randomTraceID(), randomSpanID()
}

// NewSpanID implements sdktrace.IDGenerator for a child span. The trace id is
// already fixed by the parent, so only the span id is pinned here.
func (g *IDGenerator) NewSpanID(context.Context, oteltrace.TraceID) oteltrace.SpanID {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.pinned {
		g.pinned = false
		return g.spanID
	}
	return randomSpanID()
}

func randomTraceID() oteltrace.TraceID {
	var id oteltrace.TraceID
	// crypto/rand.Read cannot fail in Go 1.24+; it panics on a broken source
	// rather than returning an error, so there is nothing to check.
	_, _ = rand.Read(id[:])
	return id
}

func randomSpanID() oteltrace.SpanID {
	var id oteltrace.SpanID
	_, _ = rand.Read(id[:])
	return id
}
