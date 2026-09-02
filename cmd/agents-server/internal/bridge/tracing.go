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

// liveSpanDataJSON bounds what goes over the WEBSOCKET: the browser parses
// every span and keeps a session's worth in memory, so this stays small.
// What it drops is still in the row, one reopen away — the row's own bound
// is per payload element (trace_span_data_kb), applied by the store.
const liveSpanDataJSON = 256 << 10

const liveOmitted = "[omitted from the live update — reopen this trace to load it]"

// wsProcessor streams spans to the client in real time: a pending version on
// span start (no ended_at — the UI renders it as in-progress) and the full
// version on span end, which is also the only one persisted. No batching: the
// consumer is a local WebSocket, and liveness is the point.
type wsProcessor struct {
	// ctx is what span persistence runs under. tracing.Processor's hooks take
	// no context, so it is captured here — detached from the run's
	// cancellation (a cancelled run still ends its spans, and they must land)
	// and carrying the configured logger.
	ctx    context.Context
	send   func(string, any)
	writer *store.SpanWriter
	runID  string
	// parentRunID is the run's lineage (a wake-up run's spawning run), stamped
	// on every span so the trace itself carries the relationship — the panel's
	// run grouping reads it here, never re-derived from task rows or
	// notification text (which a fork does not carry).
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

// cleanSpanData copies data without the redundant "name" key (span.Name
// already travels on the envelope; the data copy exists for HTTP export);
// nil when nothing is left.
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

// OnSpanStart pushes the pending span so the trace panel shows work as it
// happens. Safe to read span.Data here: the runner only annotates the span
// after the typed constructor (which fires this hook) returns.
func (p *wsProcessor) OnSpanStart(span *tracing.Span) {
	p.send(protocol.EventTraceSpan, p.spanMessage(span))
}

// OnSpanEnd pushes the finished span (same span_id — the client replaces the
// pending version) and persists it. The two are bounded SEPARATELY: the push
// is what a browser has to hold, the row is what a Replay has to read.
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
