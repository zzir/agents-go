package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/cmd/agents-server/internal/testdb"
	"github.com/zzir/agents-go/tracing"
)

// fatSpanData is a generation span's payload on a large turn: over the live cap.
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

func endedSpan(spanID string, data map[string]any) *tracing.Span {
	now := time.Now()
	return &tracing.Span{TraceID: "t1", SpanID: spanID, Name: "generation:x", Type: "generation", StartedAt: now, EndedAt: now, Data: data}
}

// The wire and the row are bounded separately: what the browser cannot hold
// is whole in the row a Replay reads, minus the redundant name.
func TestLiveSpanDataIsBoundedAndTheRowIsWhole(t *testing.T) {
	data := fatSpanData()
	live, omitted := liveSpanData(data)
	if !omitted || live["input"] != liveOmitted {
		t.Fatalf("a 400KB input must not go over the websocket, got %T", live["input"])
	}
	if _, ok := live["name"]; ok {
		t.Fatal("name should be dropped: it travels on the envelope")
	}
	if strings.Contains(liveOmitted, "trace_span_data_kb") || !strings.Contains(liveOmitted, "reopen") {
		t.Fatalf("the live marker promises a reopen, not a setting: %s", liveOmitted)
	}

	ctx := context.Background()
	traces := store.NewTraceStore(testdb.New(t))
	p := newWSProcessor(ctx, func(string, any) {}, traces, "sess", "run", "", 0, nil)
	p.OnSpanEnd(endedSpan("g1", data))

	row, err := traces.GetBySpan(ctx, "sess", "g1")
	if err != nil {
		t.Fatalf("GetBySpan: %v", err)
	}
	var back map[string]any
	if err := json.Unmarshal([]byte(row.Data), &back); err != nil {
		t.Fatalf("row data must be JSON: %v", err)
	}
	items, _ := back["input"].([]any)
	if len(items) != 1 {
		t.Fatalf("the row must keep the request Replay seeds from, got %v", back["input"])
	}
	if content, _ := items[0].(map[string]any)["content"].(string); len(content) != 400<<10 {
		t.Fatalf("stored input was truncated: %d bytes", len(content))
	}
	if _, ok := back["name"]; ok || back["response_id"] != "resp_1" {
		t.Fatalf("row metadata = %v, want name out and the rest kept", back)
	}
}

// Two calls of one conversation hash its items the same, so the second
// generation span references the first's elements instead of storing them
// again — the property the blob store rests on, checked with the SDK's own
// input items rather than maps.
func TestGenerationInputHashesAreStableAcrossCalls(t *testing.T) {
	ctx := context.Background()
	db := testdb.New(t)
	traces := store.NewTraceStore(db)
	p := newWSProcessor(ctx, func(string, any) {}, traces, "sess", "run", "", 0, nil)

	turn1 := agents.InputItemsFromText("hello")
	turn2 := slices.Concat(turn1, agents.InputItemsFromAssistantText("hi"), agents.InputItemsFromText("more"))
	tools := []map[string]any{{"name": "t", "parameters": map[string]any{"type": "object"}}}
	p.OnSpanEnd(endedSpan("g1", map[string]any{"name": "x", "input": turn1, "tools": tools}))
	p.OnSpanEnd(endedSpan("g2", map[string]any{"name": "x", "input": turn2, "tools": tools}))

	var rows []store.TraceEvent
	if err := db.NewSelect().Model(&rows).Where("session_id = ?", "sess").OrderExpr("id ASC").Scan(ctx); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || len(rows[0].Refs) != 2*32 || len(rows[1].Refs) != 4*32 {
		t.Fatalf("rows = %d, refs = %d / %d bytes; want 2 rows referencing 2 and 4 elements", len(rows), len(rows[0].Refs), len(rows[1].Refs))
	}
	if !bytes.Equal(rows[0].Refs[:32], rows[1].Refs[:32]) {
		t.Fatal("the first input item hashed differently on the second call")
	}
	if !bytes.Equal(rows[0].Refs[32:], rows[1].Refs[96:]) {
		t.Fatal("the tool definition hashed differently on the second call")
	}
	n, err := db.NewSelect().Model((*store.TraceBlob)(nil)).Where("session_id = ?", "sess").Count(ctx)
	if err != nil || n != 4 {
		t.Fatalf("blobs = %d (%v), want the 4 distinct elements", n, err)
	}
}

// Every span row carries the run's lineage, so the trace itself records which
// run a wake-up belongs to — the panel's grouping reads it here, and a fork
// (which copies trace rows but not task rows) carries it for free.
func TestSpanRowsCarryTheRunLineage(t *testing.T) {
	ctx := context.Background()
	traces := store.NewTraceStore(testdb.New(t))
	p := newWSProcessor(ctx, func(string, any) {}, traces, "sess", "run_wake", "run_origin", 0, nil)

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

// The live span event carries the images its input references, resolved — as
// run.started does for the message — while the stored payload keeps the
// reference and the row lists what rows exist.
func TestSpanEventCarriesItsAttachments(t *testing.T) {
	ctx := context.Background()
	traces := store.NewTraceStore(testdb.New(t))
	var sent []protocol.TraceSpan
	resolve := func(_ context.Context, ids []string) []protocol.AttachmentRef {
		refs := make([]protocol.AttachmentRef, 0, len(ids))
		for _, id := range ids {
			refs = append(refs, protocol.AttachmentRef{ID: id, URL: "https://cdn.example/" + id + ".png"})
		}
		return refs
	}
	p := newWSProcessor(ctx, func(_ string, v any) {
		if ts, ok := v.(protocol.TraceSpan); ok {
			sent = append(sent, ts)
		}
	}, traces, "sess", "run", "", 0, resolve)

	withImage := []any{map[string]any{"type": "message", "role": "user", "content": []any{
		map[string]any{"type": "input_text", "text": "what is this"},
		map[string]any{"type": "input_image", "image_url": store.AttachmentSentinelURL("att1"), "detail": "auto"},
	}}}
	p.OnSpanStart(endedSpan("g1", map[string]any{"name": "x"}))
	p.OnSpanEnd(endedSpan("g1", map[string]any{"name": "x", "input": withImage}))
	p.OnSpanEnd(endedSpan("g2", map[string]any{"name": "x", "input": []any{map[string]any{"role": "user", "content": "plain"}}}))
	if len(sent) != 3 || len(sent[0].Attachments) != 0 || len(sent[2].Attachments) != 0 {
		t.Fatalf("sent = %+v, want attachments only on the span that references one", sent)
	}
	if got := sent[1].Attachments; len(got) != 1 || got[0].ID != "att1" || got[0].URL != "https://cdn.example/att1.png" {
		t.Fatalf("live attachments = %+v", got)
	}
	row, err := traces.GetBySpan(ctx, "sess", "g1")
	if err != nil || !strings.Contains(row.Data, store.AttachmentSentinelURL("att1")) || len(row.Attachments) != 0 {
		t.Fatalf("row = %+v (%v): the payload keeps its reference, and no attachment row exists here", row, err)
	}
}
