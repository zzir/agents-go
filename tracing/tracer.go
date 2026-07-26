package tracing

import (
	"maps"
	"sync/atomic"
	"time"
)

// Now is the clock used for span timing. Tests may override it for determinism.
var Now = time.Now

// Tracer creates traces and spans and notifies a Processor of their lifecycle.
// A nil Tracer is a no-op, so call sites need not check for one.
type Tracer struct {
	proc Processor
}

// NewTracer returns a Tracer that reports to proc. If proc is nil the tracer is
// a no-op.
func NewTracer(proc Processor) *Tracer { return &Tracer{proc: proc} }

// TraceHandle represents an in-progress trace.
type TraceHandle struct {
	Trace  *Trace
	tracer *Tracer
	// finished is atomic: a detached child run may still be starting spans on
	// this handle while the owner finishes the trace from another goroutine.
	finished atomic.Bool
}

// SpanHandle represents an in-progress span.
type SpanHandle struct {
	Span   *Span
	tracer *Tracer
	// finished is atomic for the same reason as TraceHandle.finished.
	finished atomic.Bool
}

// TraceOption customizes a Trace before it is handed to the processor.
// Mutating a Trace after StartTrace races with background exporting; options
// are the safe way to set fields like GroupID and Metadata.
type TraceOption func(*Trace)

// WithGroupID links this trace to a group of related traces (e.g. one chat
// thread across several runs) — the counterpart of Python's RunConfig.group_id.
func WithGroupID(id string) TraceOption { return func(tr *Trace) { tr.GroupID = id } }

// WithMetadata attaches user metadata to the trace — the counterpart of
// Python's RunConfig.trace_metadata.
func WithMetadata(md map[string]any) TraceOption { return func(tr *Trace) { tr.Metadata = md } }

// StartTrace begins a new trace for the given workflow. Finish it with Finish.
func (t *Tracer) StartTrace(workflowName string, opts ...TraceOption) *TraceHandle {
	if t == nil || t.proc == nil {
		return &TraceHandle{}
	}
	tr := &Trace{TraceID: NewTraceID(), WorkflowName: workflowName}
	for _, opt := range opts {
		if opt != nil {
			opt(tr)
		}
	}
	t.proc.OnTraceStart(tr)
	return &TraceHandle{Trace: tr, tracer: t}
}

// Finish ends the trace. It is idempotent: only the first call notifies the
// processor, so deferred and explicit finishes can coexist safely.
func (h *TraceHandle) Finish() {
	if h == nil || h.tracer == nil || h.Trace == nil || !h.finished.CompareAndSwap(false, true) {
		return
	}
	h.tracer.proc.OnTraceEnd(h.Trace)
}

// startSpan is the shared constructor for StartSpan and the typed helpers.
func (h *TraceHandle) startSpan(name, parentID, spanType string, data map[string]any) *SpanHandle {
	if h == nil || h.tracer == nil || h.Trace == nil {
		return &SpanHandle{}
	}
	d := map[string]any{}
	maps.Copy(d, data)
	sp := &Span{
		TraceID:   h.Trace.TraceID,
		SpanID:    NewSpanID(),
		ParentID:  parentID,
		Name:      name,
		Type:      spanType,
		StartedAt: Now(),
		Data:      d,
	}
	h.tracer.proc.OnSpanStart(sp)
	return &SpanHandle{Span: sp, tracer: h.tracer}
}

// StartSpan begins an untyped span under this trace, optionally nested under
// parentID. Prefer a typed constructor (StartAgentSpan, etc.) where one fits.
func (h *TraceHandle) StartSpan(name, parentID string) *SpanHandle {
	return h.startSpan(name, parentID, "", nil)
}

// StartAgentSpan begins a span for an agent turn (Type SpanTypeAgent).
func (h *TraceHandle) StartAgentSpan(name, parentID string) *SpanHandle {
	return h.startSpan("agent:"+name, parentID, SpanTypeAgent, map[string]any{"name": name})
}

// StartGenerationSpan begins a span for a model call (Type SpanTypeGeneration).
// Callers typically Set("response_id", …) on the returned span.
func (h *TraceHandle) StartGenerationSpan(name, parentID string) *SpanHandle {
	return h.startSpan("generation:"+name, parentID, SpanTypeGeneration, map[string]any{"name": name})
}

// StartCompactionSpan begins a span for a session-history compaction pass
// (Type SpanTypeCompaction). The session implementation annotates it with
// before/after item counts.
func (h *TraceHandle) StartCompactionSpan(parentID string) *SpanHandle {
	return h.startSpan("compaction", parentID, SpanTypeCompaction, nil)
}

// StartFunctionSpan begins a span for a tool invocation (Type SpanTypeFunction).
func (h *TraceHandle) StartFunctionSpan(name, parentID string) *SpanHandle {
	return h.startSpan("function:"+name, parentID, SpanTypeFunction, map[string]any{"name": name})
}

// StartHandoffSpan begins a span for a handoff (Type SpanTypeHandoff).
func (h *TraceHandle) StartHandoffSpan(name, parentID string) *SpanHandle {
	return h.startSpan("handoff:"+name, parentID, SpanTypeHandoff, map[string]any{"name": name})
}

// StartGuardrailSpan begins a span for a guardrail stage ("input"/"output")
// (Type SpanTypeGuardrail).
func (h *TraceHandle) StartGuardrailSpan(stage, parentID string) *SpanHandle {
	return h.startSpan("guardrail:"+stage, parentID, SpanTypeGuardrail, map[string]any{"stage": stage})
}

// StartSpan begins a span nested under this span.
func (h *SpanHandle) StartSpan(name string) *SpanHandle {
	if h == nil || h.tracer == nil || h.Span == nil {
		return &SpanHandle{}
	}
	sp := &Span{
		TraceID:   h.Span.TraceID,
		SpanID:    NewSpanID(),
		ParentID:  h.Span.SpanID,
		Name:      name,
		StartedAt: Now(),
		Data:      map[string]any{},
	}
	h.tracer.proc.OnSpanStart(sp)
	return &SpanHandle{Span: sp, tracer: h.tracer}
}

// Set attaches a key/value to the span's data.
func (h *SpanHandle) Set(key string, value any) {
	if h == nil || h.Span == nil {
		return
	}
	if h.Span.Data == nil {
		h.Span.Data = map[string]any{}
	}
	h.Span.Data[key] = value
}

// SetError records an error on the span.
func (h *SpanHandle) SetError(message string, data map[string]any) {
	if h == nil || h.Span == nil {
		return
	}
	h.Span.Error = &SpanError{Message: message, Data: data}
}

// Finish ends the span, stamping its end time. It is idempotent: only the
// first call exports the span, so deferred and explicit finishes can coexist.
func (h *SpanHandle) Finish() {
	if h == nil || h.tracer == nil || h.Span == nil || !h.finished.CompareAndSwap(false, true) {
		return
	}
	h.Span.EndedAt = Now()
	h.tracer.proc.OnSpanEnd(h.Span)
}

// StartTypedSpan begins a typed span nested under this one.
//
// It exists so a subsystem far from the runner — an MCP client, a sandbox
// backend — can contribute a span of its own kind without the tracing package
// growing a constructor per caller.
func (h *SpanHandle) StartTypedSpan(name, spanType string, data map[string]any) *SpanHandle {
	if h == nil || h.tracer == nil || h.Span == nil {
		return &SpanHandle{}
	}
	if data == nil {
		data = map[string]any{}
	}
	sp := &Span{
		TraceID:   h.Span.TraceID,
		SpanID:    NewSpanID(),
		ParentID:  h.Span.SpanID,
		Name:      name,
		Type:      spanType,
		StartedAt: Now(),
		Data:      data,
	}
	h.tracer.proc.OnSpanStart(sp)
	return &SpanHandle{Span: sp, tracer: h.tracer}
}
