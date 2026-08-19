package bridge

import (
	"context"
	"encoding/json"
	"maps"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/tracing"
)

// The two caps on span data, which are separate because their consumers are.
// A generation span carries the full model request and response, so on a large
// turn it is the biggest thing either path handles.
const (
	// liveSpanDataJSON bounds what goes over the WEBSOCKET. The browser parses
	// every span and keeps the whole session's worth in memory, so this stays
	// small; what it drops is still in the row below, one reopen away.
	liveSpanDataJSON = 256 << 10
	// storedSpanDataJSON is the default bound on what goes into trace_events —
	// read one span at a time, on demand, and the only copy Replay can seed a
	// re-run from. Overridable through the trace_span_data_kb setting.
	storedSpanDataJSON = 8 << 20
)

// Markers for what a cap removed. They differ because the remedies do: the
// live one is recoverable by reopening the trace, the stored one is gone.
const (
	liveOmitted   = "[omitted from the live update — reopen this trace to load it]"
	storedOmitted = "[omitted: over the stored span limit (trace_span_data_kb)]"
)

// wsProcessor streams spans to the client in real time: a pending version on
// span start (no ended_at — the UI renders it as in-progress) and the full
// version on span end, which is also the only one persisted. No batching: the
// consumer is a local WebSocket, and liveness is the point.
type wsProcessor struct {
	// ctx is what span persistence runs under. tracing.Processor's hooks take
	// no context, so it is captured here — detached from the run's
	// cancellation (a cancelled run still ends its spans, and they must land)
	// and carrying the configured logger.
	ctx       context.Context
	send      func(string, any)
	traces    *store.TraceStore
	sessionID string
	runID     string
	// parentRunID is the run's lineage (a wake-up run's spawning run), stamped
	// on every span so the trace itself carries the relationship — the panel's
	// run grouping reads it here, never re-derived from task rows or
	// notification text (which a fork does not carry).
	parentRunID string
	// storedCap is the run's resolved trace_span_data_kb, read once when the
	// run starts rather than per span.
	storedCap int
}

func newWSProcessor(ctx context.Context, send func(string, any), traces *store.TraceStore, sessionID, runID, parentRunID string, storedCap int) *wsProcessor {
	return &wsProcessor{
		ctx:         context.WithoutCancel(ctx),
		send:        send,
		traces:      traces,
		sessionID:   sessionID,
		runID:       runID,
		parentRunID: parentRunID,
		storedCap:   storedCap,
	}
}

// boundSpanData prepares span data for one consumer: the redundant "name" key
// is dropped (span.Name already travels on the envelope; the data copy exists
// for HTTP export), and past limit the bulky payload fields are replaced with
// marker. Returns the cleaned data map, its JSON, and whether any field was
// replaced.
func boundSpanData(data map[string]any, limit int, marker string) (cleaned map[string]any, raw string, omitted bool) {
	if len(data) == 0 {
		return nil, "", false
	}
	cleaned = make(map[string]any, len(data))
	maps.Copy(cleaned, data)
	delete(cleaned, "name")
	if len(cleaned) == 0 {
		return nil, "", false
	}
	b, err := json.Marshal(cleaned)
	if err != nil {
		return cleaned, "", false
	}
	if len(b) <= limit {
		return cleaned, string(b), false
	}
	for _, k := range []string{"input", "output", "system_instructions", "tools"} {
		if _, ok := cleaned[k]; ok {
			cleaned[k] = marker
			omitted = true
		}
	}
	b, err = json.Marshal(cleaned)
	if err != nil {
		return cleaned, "", omitted
	}
	return cleaned, string(b), omitted
}

// spanMessage is what the CLIENT gets: bounded for the wire.
func (p *wsProcessor) spanMessage(span *tracing.Span) protocol.TraceSpan {
	data, _, omitted := boundSpanData(span.Data, liveSpanDataJSON, liveOmitted)
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
	_, dataJSON, _ := boundSpanData(span.Data, p.storedCap, storedOmitted)
	te := &store.TraceEvent{
		SessionID:   p.sessionID,
		RunID:       p.runID,
		ParentRunID: p.parentRunID,
		Kind:        "span",
		SpanID:      span.SpanID,
		ParentID:    span.ParentID,
		Name:        span.Name,
		Detail:      span.Type,
		Error:       ts.Error,
		Data:        dataJSON,
		StartedAt:   ts.StartedAt,
		EndedAt:     ts.EndedAt,
	}
	if err := p.traces.Insert(p.ctx, te); err != nil {
		zerolog.Ctx(p.ctx).Warn().Err(err).Msg("failed to persist trace span")
	}
}

func (p *wsProcessor) ForceFlush()              {}
func (p *wsProcessor) Shutdown(context.Context) {}

var _ tracing.Processor = (*wsProcessor)(nil)

func newTracer(ctx context.Context, send func(string, any), traces *store.TraceStore, sessionID, runID, parentRunID string, storedCap int) *tracing.Tracer {
	return tracing.NewTracer(newWSProcessor(ctx, send, traces, sessionID, runID, parentRunID, storedCap))
}

// spanDataCap resolves the trace_span_data_kb setting: a positive number of
// kilobytes, anything else the default. Read once per run.
func spanDataCap(ctx context.Context, settings *store.SettingStore) int {
	kb, err := strconv.Atoi(strings.TrimSpace(settingValue(ctx, settings, "trace_span_data_kb")))
	if err != nil || kb <= 0 {
		return storedSpanDataJSON
	}
	return kb << 10
}
