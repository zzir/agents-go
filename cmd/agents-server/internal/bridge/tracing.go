package bridge

import (
	"context"
	"encoding/json"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/tracing"
)

type wsExporter struct {
	send      func(string, any)
	traces    *store.TraceStore
	sessionID string
	runID     string
}

func newWSExporter(send func(string, any), traces *store.TraceStore, sessionID, runID string) *wsExporter {
	return &wsExporter{
		send:      send,
		traces:    traces,
		sessionID: sessionID,
		runID:     runID,
	}
}

func (e *wsExporter) Export(items []any) {
	var batch []store.TraceEvent
	for _, item := range items {
		span, ok := item.(*tracing.Span)
		if !ok {
			continue
		}
		ts := protocol.TraceSpan{
			TraceID:   span.TraceID,
			SpanID:    span.SpanID,
			ParentID:  span.ParentID,
			Name:      span.Name,
			Type:      span.Type,
			StartedAt: span.StartedAt.Format(time.RFC3339Nano),
			Data:      span.Data,
		}
		if !span.EndedAt.IsZero() {
			ts.EndedAt = span.EndedAt.Format(time.RFC3339Nano)
		}
		e.send("trace.span", ts)

		dataJSON := ""
		if len(span.Data) > 0 {
			if b, err := json.Marshal(span.Data); err == nil {
				dataJSON = string(b)
			}
		}
		batch = append(batch, store.TraceEvent{
			SessionID: e.sessionID,
			RunID:     e.runID,
			Kind:      "span",
			SpanID:    span.SpanID,
			ParentID:  span.ParentID,
			Name:      span.Name,
			Detail:    span.Type,
			Data:      dataJSON,
			StartedAt: span.StartedAt.Format(time.RFC3339Nano),
			EndedAt:   ts.EndedAt,
		})
	}
	if len(batch) > 0 {
		if err := e.traces.InsertBatch(context.Background(), batch); err != nil {
			log.Warn().Err(err).Msg("failed to persist trace spans")
		}
	}
}

func newTracer(send func(string, any), traces *store.TraceStore, sessionID, runID string) *tracing.Tracer {
	exp := newWSExporter(send, traces, sessionID, runID)
	proc := tracing.NewBatchProcessor(exp, tracing.BatchProcessorOptions{
		MaxBatchSize:  32,
		FlushInterval: time.Second,
	})
	return tracing.NewTracer(proc)
}
