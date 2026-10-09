// Package tracing provides traces and spans for observing agent runs, plus
// processors and exporters for shipping that telemetry elsewhere.
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

// Span types set by the typed span constructors; consumers dispatch on Span.Type.
const (
	SpanTypeAgent      = "agent"
	SpanTypeGeneration = "generation"
	SpanTypeFunction   = "function"
	SpanTypeHandoff    = "handoff"
	SpanTypeGuardrail  = "guardrail"
	SpanTypeCompaction = "compaction"
	// SpanTypeModelRetry marks one failed model attempt under its generation
	// span (spec §2.11e).
	SpanTypeModelRetry = "model_retry"
	// SpanTypeMCP is an MCP server round trip: listing tools, or calling one.
	SpanTypeMCP = "mcp"
	// SpanTypeSandbox is a sandbox operation: a command, a patch, a file read or write.
	SpanTypeSandbox = "sandbox"
)

// Span is a single unit of work within a trace (an agent turn, a model
// generation, a tool call, etc).
type Span struct {
	TraceID  string
	SpanID   string
	ParentID string
	Name     string
	// Type is a SpanType constant, or empty for a span from the untyped StartSpan.
	Type      string
	StartedAt time.Time
	EndedAt   time.Time
	Error     *SpanError
	// Data holds the span's structured fields ("name", "stage", "response_id", …).
	Data map[string]any
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

// Exporter ships finished traces and spans to a destination. Export may run
// concurrently (a periodic flush overlapping ForceFlush); a batch is delivered
// once, in no guaranteed order. Each Item is a *Trace or a *Span, so type-switch:
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

// randHex returns 2n random hex characters; crypto/rand.Read cannot fail (Go 1.24+).
func randHex(n int) string {
	buf := make([]byte, n)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

// NewTraceID returns a fresh trace identifier: "trace_" followed by 32 hex characters.
func NewTraceID() string { return "trace_" + randHex(16) }

// NewSpanID returns a fresh span identifier: "span_" followed by 16 hex
// characters, the OpenTelemetry width — see decisions §5.6b.
func NewSpanID() string { return "span_" + randHex(8) }
