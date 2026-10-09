package tracing

import (
	"maps"
	"sync"
	"sync/atomic"
	"time"
)

// Now is the clock used for span timing. Tests may override it for determinism.
var Now = time.Now

// Tracer creates traces and spans and notifies a Processor of their lifecycle.
// A nil Tracer is a no-op.
type Tracer struct {
	proc Processor
}

// NewTracer returns a Tracer reporting to proc; a nil proc makes it a no-op.
func NewTracer(proc Processor) *Tracer { return &Tracer{proc: proc} }

// TraceHandle represents an in-progress trace.
type TraceHandle struct {
	Trace  *Trace
	tracer *Tracer
	// finished is atomic: a detached child run may start spans while another
	// goroutine finishes.
	finished atomic.Bool
}

// SpanHandle represents an in-progress span.
type SpanHandle struct {
	Span   *Span
	tracer *Tracer
	// finished is atomic for the same reason as TraceHandle.finished.
	finished atomic.Bool
	// mu guards Data and EndedAt against Finish's handover — see spec §2.11e.
	mu sync.Mutex
}

// TraceOption customizes a Trace before it reaches the processor; mutating one
// after StartTrace races the export (spec §2.11e).
type TraceOption func(*Trace)

// WithGroupID links this trace to a group of related traces (e.g. one chat thread).
func WithGroupID(id string) TraceOption { return func(tr *Trace) { tr.GroupID = id } }

// WithMetadata attaches user metadata to the trace.
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

// Finish ends the trace; idempotent, only the first call notifies the processor.
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

// StartSpan begins an untyped span under this trace, nested under parentID when
// non-empty; prefer a typed constructor where one fits.
func (h *TraceHandle) StartSpan(name, parentID string) *SpanHandle {
	return h.startSpan(name, parentID, "", nil)
}

// StartAgentSpan begins a span for an agent turn (Type SpanTypeAgent).
func (h *TraceHandle) StartAgentSpan(name, parentID string) *SpanHandle {
	return h.startSpan("agent:"+name, parentID, SpanTypeAgent, map[string]any{"name": name})
}

// StartGenerationSpan begins a span for a model call (Type SpanTypeGeneration).
func (h *TraceHandle) StartGenerationSpan(name, parentID string) *SpanHandle {
	return h.startSpan("generation:"+name, parentID, SpanTypeGeneration, map[string]any{"name": name})
}

// StartCompactionSpan begins a span for a compaction pass (Type SpanTypeCompaction).
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

// Set attaches a key/value to the span's data; after Finish it is ignored, safely
// from any goroutine — see spec §2.11e.
func (h *SpanHandle) Set(key string, value any) {
	if h == nil || h.Span == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.finished.Load() {
		return
	}
	if h.Span.Data == nil {
		h.Span.Data = map[string]any{}
	}
	h.Span.Data[key] = value
}

// SetError records an error on the span. Like Set, it is ignored after Finish.
func (h *SpanHandle) SetError(message string, data map[string]any) {
	if h == nil || h.Span == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.finished.Load() {
		return
	}
	h.Span.Error = &SpanError{Message: message, Data: data}
}

// Finish ends the span, stamping its end time; idempotent, only the first call exports.
func (h *SpanHandle) Finish() {
	if h == nil || h.tracer == nil || h.Span == nil || !h.finished.CompareAndSwap(false, true) {
		return
	}
	// Under mu: an annotation in flight lands before the handover or is dropped.
	h.mu.Lock()
	h.Span.EndedAt = Now()
	h.mu.Unlock()
	h.tracer.proc.OnSpanEnd(h.Span)
}

// StartTypedSpan begins a typed span nested under this one, for a subsystem
// (MCP, sandbox) contributing a span kind of its own.
func (h *SpanHandle) StartTypedSpan(name, spanType string, data map[string]any) *SpanHandle {
	if h == nil || h.tracer == nil || h.Span == nil {
		return &SpanHandle{}
	}
	d := map[string]any{}
	maps.Copy(d, data)
	sp := &Span{
		TraceID:   h.Span.TraceID,
		SpanID:    NewSpanID(),
		ParentID:  h.Span.SpanID,
		Name:      name,
		Type:      spanType,
		StartedAt: Now(),
		Data:      d,
	}
	h.tracer.proc.OnSpanStart(sp)
	return &SpanHandle{Span: sp, tracer: h.tracer}
}
