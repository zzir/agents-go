package tracing

import "time"

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
	Trace    *Trace
	tracer   *Tracer
	finished bool
}

// SpanHandle represents an in-progress span.
type SpanHandle struct {
	Span     *Span
	tracer   *Tracer
	finished bool
}

// StartTrace begins a new trace for the given workflow. Finish it with Finish.
func (t *Tracer) StartTrace(workflowName string) *TraceHandle {
	if t == nil || t.proc == nil {
		return &TraceHandle{}
	}
	tr := &Trace{TraceID: NewTraceID(), WorkflowName: workflowName}
	t.proc.OnTraceStart(tr)
	return &TraceHandle{Trace: tr, tracer: t}
}

// Finish ends the trace. It is idempotent: only the first call notifies the
// processor, so deferred and explicit finishes can coexist safely.
func (h *TraceHandle) Finish() {
	if h == nil || h.tracer == nil || h.Trace == nil || h.finished {
		return
	}
	h.finished = true
	h.tracer.proc.OnTraceEnd(h.Trace)
}

// StartSpan begins a span under this trace, optionally nested under parentID.
func (h *TraceHandle) StartSpan(name, parentID string) *SpanHandle {
	if h == nil || h.tracer == nil || h.Trace == nil {
		return &SpanHandle{}
	}
	sp := &Span{
		TraceID:   h.Trace.TraceID,
		SpanID:    NewSpanID(),
		ParentID:  parentID,
		Name:      name,
		StartedAt: Now(),
		Data:      map[string]any{},
	}
	h.tracer.proc.OnSpanStart(sp)
	return &SpanHandle{Span: sp, tracer: h.tracer}
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
	if h == nil || h.tracer == nil || h.Span == nil || h.finished {
		return
	}
	h.finished = true
	h.Span.EndedAt = Now()
	h.tracer.proc.OnSpanEnd(h.Span)
}
