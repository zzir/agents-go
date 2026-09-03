package bridge

import (
	"context"
	"encoding/json"
	"maps"
	"time"

	"github.com/zzir/agents-go/cmd/agents-server/internal/logging"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/tracing"
)

// liveSpanDataJSON bounds what goes over the WEBSOCKET (the browser keeps a
// session's worth); the row's own bound is per element, applied by the store.
const liveSpanDataJSON = 256 << 10

const liveOmitted = "[omitted from the live update — reopen this trace to load it]"

// wsProcessor streams spans to the client: a pending version on start, the
// full version on end (the only one persisted). No batching — liveness is the point.
type wsProcessor struct {
	// ctx is what span persistence runs under: tracing.Processor's hooks take
	// no context, so it is captured — detached from the run's cancellation.
	ctx    context.Context
	send   func(string, any)
	writer *store.SpanWriter
	runID  string
	// parentRunID is the run's lineage (a wake-up run's spawning run), stamped
	// on every span so the trace itself carries it (a fork does not).
	parentRunID string
}

// newWSProcessor returns the processor of one run; elemCap is the run's
// resolved trace_span_data_kb in bytes, read once rather than per span.
func newWSProcessor(ctx context.Context, send func(string, any), traces *store.TraceStore, sessionID, runID, parentRunID string, elemCap int) *wsProcessor {
	return &wsProcessor{
		ctx:         context.WithoutCancel(ctx),
		send:        send,
		writer:      traces.NewSpanWriter(sessionID, elemCap),
		runID:       runID,
		parentRunID: parentRunID,
	}
}

// cleanSpanData copies data without the redundant "name" key (span.Name is on
// the envelope); nil when nothing is left.
func cleanSpanData(data map[string]any) map[string]any {
	if len(data) == 0 {
		return nil
	}
	cleaned := make(map[string]any, len(data))
	maps.Copy(cleaned, data)
	delete(cleaned, "name")
	if len(cleaned) == 0 {
		return nil
	}
	return cleaned
}

// liveSpanData bounds the data for the wire: past liveSpanDataJSON the bulky
// payload fields are replaced with liveOmitted. Reports whether any was.
func liveSpanData(data map[string]any) (map[string]any, bool) {
	cleaned := cleanSpanData(data)
	if cleaned == nil {
		return nil, false
	}
	b, err := json.Marshal(cleaned)
	if err != nil || len(b) <= liveSpanDataJSON {
		return cleaned, false
	}
	omitted := false
	for _, k := range []string{"input", "output", "system_instructions", "tools"} {
		if _, ok := cleaned[k]; ok {
			cleaned[k] = liveOmitted
			omitted = true
		}
	}
	return cleaned, omitted
}

// spanDataJSON is the row's data document: cleaned and whole — the store
// splits its payload out and caps the elements.
func spanDataJSON(data map[string]any) string {
	cleaned := cleanSpanData(data)
	if cleaned == nil {
		return ""
	}
	b, err := json.Marshal(cleaned)
	if err != nil {
		return ""
	}
	return string(b)
}

// spanMessage is what the CLIENT gets: bounded for the wire.
func (p *wsProcessor) spanMessage(span *tracing.Span) protocol.TraceSpan {
	data, omitted := liveSpanData(span.Data)
	ts := protocol.TraceSpan{
		RunID:          p.runID,
		ParentRunID:    p.parentRunID,
		TraceID:        span.TraceID,
		SpanID:         span.SpanID,
		ParentID:       span.ParentID,
		Name:           span.Name,
		Type:           span.Type,
		StartedAt:      span.StartedAt.Format(time.RFC3339Nano),
		Data:           data,
		PayloadOmitted: omitted,
	}
	if span.Error != nil {
		ts.Error = span.Error.Message
	}
	if !span.EndedAt.IsZero() {
		ts.EndedAt = span.EndedAt.Format(time.RFC3339Nano)
	}
	return ts
}

func (p *wsProcessor) OnTraceStart(*tracing.Trace) {}
func (p *wsProcessor) OnTraceEnd(*tracing.Trace)   {}

// OnSpanStart pushes the pending span. span.Data is safe to read: the runner
// annotates the span only after the typed constructor returns.
func (p *wsProcessor) OnSpanStart(span *tracing.Span) {
	p.send(protocol.EventTraceSpan, p.spanMessage(span))
}

// OnSpanEnd pushes the finished span (same span_id; the client replaces the
// pending one) and persists it, each bounded on its own.
func (p *wsProcessor) OnSpanEnd(span *tracing.Span) {
	ts := p.spanMessage(span)
	p.send(protocol.EventTraceSpan, ts)
	te := &store.TraceEvent{
		RunID:       p.runID,
		ParentRunID: p.parentRunID,
		Kind:        "span",
		SpanID:      span.SpanID,
		ParentID:    span.ParentID,
		Name:        span.Name,
		Detail:      span.Type,
		Error:       ts.Error,
		Data:        spanDataJSON(span.Data),
		StartedAt:   ts.StartedAt,
		EndedAt:     ts.EndedAt,
	}
	if err := p.writer.Insert(p.ctx, te); err != nil {
		logging.Ctx(p.ctx).Warn("failed to persist trace span", "error", err)
	}
}

func (p *wsProcessor) ForceFlush()              {}
func (p *wsProcessor) Shutdown(context.Context) {}

var _ tracing.Processor = (*wsProcessor)(nil)

func newTracer(ctx context.Context, send func(string, any), traces *store.TraceStore, sessionID, runID, parentRunID string, elemCap int) *tracing.Tracer {
	return tracing.NewTracer(newWSProcessor(ctx, send, traces, sessionID, runID, parentRunID, elemCap))
}
