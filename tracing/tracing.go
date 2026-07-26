// Package tracing provides traces and spans for observing agent runs, plus
// processors and exporters for shipping that telemetry elsewhere. It is the Go
// counterpart of the Python SDK's tracing subsystem.
package tracing

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"time"
)

// Trace is a top-level workflow trace grouping the spans of one or more runs.
type Trace struct {
	TraceID      string
	WorkflowName string
	GroupID      string
	Metadata     map[string]any
}

func (*Trace) isTraceItem() {}

// SpanError describes an error attached to a span.
type SpanError struct {
	Message string
	Data    map[string]any
}

// Span types attached by the typed span constructors (StartAgentSpan, etc.) so
// consumers can dispatch on Span.Type instead of parsing Span.Name.
const (
	SpanTypeAgent      = "agent"
	SpanTypeGeneration = "generation"
	SpanTypeFunction   = "function"
	SpanTypeHandoff    = "handoff"
	SpanTypeGuardrail  = "guardrail"
	SpanTypeCompaction = "compaction"
	// SpanTypeModelRetry is one retried model call, nested under the
	// generation span it belongs to. A generation span that took eight seconds
	// because it was tried three times is otherwise indistinguishable from one
	// that was simply slow.
	SpanTypeModelRetry = "model_retry"
	// SpanTypeMCP is an MCP server round trip: listing tools, or calling one.
	SpanTypeMCP = "mcp"
	// SpanTypeSandbox is a sandbox operation: running a command, applying a
	// patch, reading or writing a file.
	SpanTypeSandbox = "sandbox"
)

// Span is a single unit of work within a trace (an agent turn, a model
// generation, a tool call, etc).
type Span struct {
	TraceID  string
	SpanID   string
	ParentID string
	Name     string
	// Type is one of the SpanType constants when the span was created via a typed
	// constructor; it is empty for spans from the untyped StartSpan. Data holds
	// the span's structured fields (e.g. "name", "stage", "response_id").
	Type      string
	StartedAt time.Time
	EndedAt   time.Time
	Error     *SpanError
	Data      map[string]any
}

func (*Span) isTraceItem() {}

// Processor receives trace and span lifecycle notifications. Implementations
// must be safe for concurrent use.
type Processor interface {
	OnTraceStart(trace *Trace)
	OnTraceEnd(trace *Trace)
	OnSpanStart(span *Span)
	OnSpanEnd(span *Span)
	// ForceFlush blocks until buffered telemetry has been exported.
	ForceFlush()
	// Shutdown flushes and releases resources, honoring ctx for deadlines.
	Shutdown(ctx context.Context)
}

// Exporter ships finished traces and spans to a destination.
//
// Export may be called concurrently (a periodic flush and an explicit
// ForceFlush/Shutdown can overlap), so implementations must be safe for
// concurrent use. Batches are never delivered twice, but ordering across
// concurrent Export calls is not guaranteed.
//
// Each element of the items slice is an Item — either a *Trace (the root of a
// trace tree) or a *Span (a single unit of work within a trace). The Item union
// is sealed, so implementations should type-switch to distinguish them:
//
//	func (e *myExporter) Export(items []Item) {
//	    for _, item := range items {
//	        switch v := item.(type) {
//	        case *Trace:
//	            // handle trace
//	        case *Span:
//	            // handle span
//	        }
//	    }
//	}
type Exporter interface {
	Export(items []Item)
}

// randHex returns 2n random hex characters from n crypto/rand bytes. As of
// Go 1.24 crypto/rand.Read never fails (it aborts the program if the OS source
// is unavailable), and it is cheap and safe for concurrent use.
func randHex(n int) string {
	buf := make([]byte, n)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

// NewTraceID returns a fresh trace identifier: "trace_" followed by 32 hex
// characters, matching the Python SDK's format.
func NewTraceID() string { return "trace_" + randHex(16) }

// NewSpanID returns a fresh span identifier: "span_" followed by 16 hex
// characters.
//
// The width is 8 bytes because that is what an OpenTelemetry span id is, and
// tracing/otel rebuilds our tree as OTel spans by reusing these ids verbatim.
// A wider id would have to be truncated there — silently, and differently for
// each exporter that tried. Trace ids are 16 bytes, which already matches.
func NewSpanID() string { return "span_" + randHex(8) }
