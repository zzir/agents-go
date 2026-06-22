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
// Each element of the items slice is either a *Trace (the root of a trace tree)
// or a *Span (a single unit of work within a trace). Implementations should
// type-switch to distinguish them:
//
//	func (e *myExporter) Export(items []any) {
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
	Export(items []any)
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

// NewSpanID returns a fresh span identifier: "span_" followed by 24 hex
// characters, matching the Python SDK's format.
func NewSpanID() string { return "span_" + randHex(12) }
