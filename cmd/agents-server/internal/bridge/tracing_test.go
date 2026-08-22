package bridge

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/tracing"
)

// fatSpanData is a generation span's payload on a large turn: over the live cap,
// under the stored one.
func fatSpanData() map[string]any {
	return map[string]any{
		"name":                "gpt-5.5",
		"response_id":         "resp_1",
		"system_instructions": "you are helpful",
		"input":               []any{map[string]any{"role": "user", "content": strings.Repeat("x", 400<<10)}},
		"tools":               []any{map[string]any{"name": "exec_command"}},
		"input_tokens":        120000,
	}
}

// The wire and the row are bounded separately: what the browser cannot hold is
// still in the row a Replay reads.
func TestSpanDataCapsAreSeparate(t *testing.T) {
	data := fatSpanData()

	live, liveJSON, _ := boundSpanData(data, liveSpanDataJSON, liveOmitted)
	if live["input"] != liveOmitted {
		t.Fatalf("a 400KB input must not go over the websocket, got %T", live["input"])
	}
	if len(liveJSON) > liveSpanDataJSON {
		t.Fatalf("bounded live payload is still %d bytes", len(liveJSON))
	}

	stored, storedJSON, _ := boundSpanData(data, storedSpanDataJSON, storedOmitted)
	items, ok := stored["input"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("the row must keep the request Replay seeds from, got %T", stored["input"])
	}
	if len(storedJSON) < 400<<10 {
		t.Fatalf("stored payload was truncated: %d bytes", len(storedJSON))
	}

	// Same span, two answers — that is the point of the split.
	if liveJSON == storedJSON {
		t.Fatal("live and stored payloads should differ for a span this size")
	}
}

// Over the stored cap the marker says so, and says what to change: the two
// markers are the difference between "reopen it" and "it is gone".
func TestStoredCapMarkerNamesTheSetting(t *testing.T) {
	_, storedJSON, omitted := boundSpanData(fatSpanData(), 1<<10, storedOmitted)
	if !strings.Contains(storedJSON, "trace_span_data_kb") {
		t.Fatalf("the stored marker should name the setting that lifts it: %s", storedJSON)
	}
	if !omitted {
		t.Fatal("a bounded span must say its payload was replaced")
	}
	if strings.Contains(storedJSON, "reopen") {
		t.Fatal("the stored marker must not promise a reopen would bring it back")
	}
}

// The redundant "name" key is dropped whatever the caps: it already travels on
// the envelope.
func TestSpanDataDropsTheRedundantName(t *testing.T) {
	cleaned, raw, _ := boundSpanData(map[string]any{"name": "gpt-5.5", "response_id": "resp_1"}, storedSpanDataJSON, storedOmitted)
	if _, ok := cleaned["name"]; ok {
		t.Fatalf("name should be dropped, got %v", cleaned)
	}
	var back map[string]any
	if err := json.Unmarshal([]byte(raw), &back); err != nil {
		t.Fatalf("bounded data must stay valid JSON: %v", err)
	}
	if back["response_id"] != "resp_1" {
		t.Fatalf("everything else survives, got %v", back)
	}
}

// Every span row carries the run's lineage, so the trace itself records which
// run a wake-up belongs to — the panel's grouping reads it here, and a fork
// (which copies trace rows but not task rows) carries it for free.
func TestSpanRowsCarryTheRunLineage(t *testing.T) {
	ctx := context.Background()
	traces := store.NewTraceStore(newTestDB(t))
	p := newWSProcessor(ctx, func(string, any) {}, traces, "sess", "run_wake", "run_origin", storedSpanDataJSON)

	now := time.Now()
	p.OnSpanEnd(&tracing.Span{TraceID: "t1", SpanID: "s1", Name: "agent:x", Type: "agent", StartedAt: now, EndedAt: now})

	rows, err := traces.ListBySession(ctx, "sess", "", 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0].ParentRunID != "run_origin" {
		t.Fatalf("span row lineage = %q, want run_origin", rows[0].ParentRunID)
	}
}
