package bridge

import (
	"context"
	"encoding/json"
	"maps"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/tracing"
)

// maxSpanDataJSON caps the serialized span data pushed over the WS and stored
// in trace_events. Generation spans carry the full model request/response;
// past the cap the bulky payload fields are replaced with a marker.
const maxSpanDataJSON = 512 << 10

// wsProcessor streams spans to the client in real time: a pending version on
// span start (no ended_at — the UI renders it as in-progress) and the full
// version on span end, which is also the only one persisted. No batching: the
// consumer is a local WebSocket, and liveness is the point.
type wsProcessor struct {
	send      func(string, any)
	traces    *store.TraceStore
	sessionID string
	runID     string
}

func newWSProcessor(send func(string, any), traces *store.TraceStore, sessionID, runID string) *wsProcessor {
	return &wsProcessor{
		send:      send,
		traces:    traces,
		sessionID: sessionID,
		runID:     runID,
	}
}

// boundSpanData prepares span data for the client: the redundant "name" key
// is dropped (span.Name already travels on the envelope; the data copy exists
// for Python-parity HTTP export), and the bulky payload fields (model
// input/output) are replaced with a marker when the JSON exceeds
// maxSpanDataJSON. Returns the cleaned data map and its JSON.
func boundSpanData(data map[string]any) (map[string]any, string) {
	if len(data) == 0 {
		return nil, ""
	}
	cleaned := make(map[string]any, len(data))
	maps.Copy(cleaned, data)
	delete(cleaned, "name")
	if len(cleaned) == 0 {
		return nil, ""
	}
	b, err := json.Marshal(cleaned)
	if err != nil {
		return cleaned, ""
	}
	if len(b) <= maxSpanDataJSON {
		return cleaned, string(b)
	}
	for _, k := range []string{"input", "output", "system_instructions", "tools"} {
		if _, ok := cleaned[k]; ok {
			cleaned[k] = "[omitted: span data exceeds 512KB]"
		}
	}
	b, err = json.Marshal(cleaned)
	if err != nil {
		return cleaned, ""
	}
	return cleaned, string(b)
}

func (p *wsProcessor) spanMessage(span *tracing.Span) (protocol.TraceSpan, string) {
	data, dataJSON := boundSpanData(span.Data)
	ts := protocol.TraceSpan{
		RunID:     p.runID,
		TraceID:   span.TraceID,
		SpanID:    span.SpanID,
		ParentID:  span.ParentID,
		Name:      span.Name,
		Type:      span.Type,
		StartedAt: span.StartedAt.Format(time.RFC3339Nano),
		Data:      data,
	}
	if span.Error != nil {
		ts.Error = span.Error.Message
	}
	if !span.EndedAt.IsZero() {
		ts.EndedAt = span.EndedAt.Format(time.RFC3339Nano)
	}
	return ts, dataJSON
}

func (p *wsProcessor) OnTraceStart(*tracing.Trace) {}
func (p *wsProcessor) OnTraceEnd(*tracing.Trace)   {}

// OnSpanStart pushes the pending span so the trace panel shows work as it
// happens. Safe to read span.Data here: the runner only annotates the span
// after the typed constructor (which fires this hook) returns.
func (p *wsProcessor) OnSpanStart(span *tracing.Span) {
	ts, _ := p.spanMessage(span)
	p.send("trace.span", ts)
}

// OnSpanEnd pushes the finished span (same span_id — the client replaces the
// pending version) and persists it.
func (p *wsProcessor) OnSpanEnd(span *tracing.Span) {
	ts, dataJSON := p.spanMessage(span)
	p.send("trace.span", ts)
	te := &store.TraceEvent{
		SessionID: p.sessionID,
		RunID:     p.runID,
		Kind:      "span",
		SpanID:    span.SpanID,
		ParentID:  span.ParentID,
		Name:      span.Name,
		Detail:    span.Type,
		Error:     ts.Error,
		Data:      dataJSON,
		StartedAt: ts.StartedAt,
		EndedAt:   ts.EndedAt,
	}
	if err := p.traces.Insert(context.Background(), te); err != nil {
		log.Warn().Err(err).Msg("failed to persist trace span")
	}
}

func (p *wsProcessor) ForceFlush()              {}
func (p *wsProcessor) Shutdown(context.Context) {}

var _ tracing.Processor = (*wsProcessor)(nil)

func newTracer(send func(string, any), traces *store.TraceStore, sessionID, runID string) *tracing.Tracer {
	return tracing.NewTracer(newWSProcessor(send, traces, sessionID, runID))
}
