package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

// The summary listing keeps what a row shows (tokens, model, error) and leaves
// the payload out — the model request and reply, a tool's arguments and result
// — marking the rows that had any, and GetBySpan serves the whole row for one
// of them, its payload rebuilt from trace_blobs. A row that is not JSON passes
// through untouched.
func TestTraceSummaryListingLeavesThePayloadOut(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	ts := NewTraceStore(db)
	id := ids(t)
	gen := &TraceEvent{SessionID: id("s1"), RunID: id("r1"), Kind: "span", SpanID: "sp-gen", Name: "generation", Detail: "generation",
		Data: `{"model":"gpt-test","input_tokens":12,"output_tokens":3,"input":[{"role":"user","content":"a very long history"}],"output":[{"type":"message"}],"system_instructions":"be brief","tools":[{"name":"t"}]}`}
	fn := &TraceEvent{SessionID: id("s1"), RunID: id("r1"), Kind: "span", SpanID: "sp-fn", Name: "function:exec", Detail: "function",
		Data: `{"input":"{\"cmd\":\"ls\"}","output":"a b c"}`}
	agent := &TraceEvent{SessionID: id("s1"), RunID: id("r1"), Kind: "span", SpanID: "sp-agent", Name: "agent:coder", Detail: "agent",
		Data: `{"handoffs":[],"agent_name":"coder"}`}
	compaction := &TraceEvent{SessionID: id("s1"), RunID: id("r1"), Kind: "span", SpanID: "sp-compact", Name: "compaction", Detail: "compaction",
		Data: `{"before_items":3,"after_items":1}`}
	legacy := &TraceEvent{SessionID: id("s1"), RunID: id("r1"), Kind: "span", SpanID: "sp-legacy", Name: "old", Data: "not json"}
	other := &TraceEvent{SessionID: id("s2"), RunID: id("r9"), Kind: "span", SpanID: "sp-gen", Name: "generation", Data: `{"input":"x"}`}
	genDoc := gen.Data // Insert leaves the row's metadata in Data
	for _, ev := range []*TraceEvent{gen, fn, agent, compaction, legacy, other} {
		if err := ts.Insert(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}

	rows, err := ts.ListSummaryBySession(ctx, id("s1"), "", 0)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if len(rows) != 5 {
		t.Fatalf("rows = %d, want 5", len(rows))
	}
	byID := map[string]TraceEvent{}
	for _, r := range rows {
		byID[r.SpanID] = r
	}
	var genData map[string]any
	if err := json.Unmarshal([]byte(byID["sp-gen"].Data), &genData); err != nil {
		t.Fatalf("summary data is not JSON: %v (%s)", err, byID["sp-gen"].Data)
	}
	for _, gone := range []string{"input", "output", "system_instructions", "tools"} {
		if _, ok := genData[gone]; ok {
			t.Errorf("summary still carries %q", gone)
		}
	}
	if genData["model"] != "gpt-test" || genData["input_tokens"] != float64(12) || !byID["sp-gen"].PayloadOmitted {
		t.Fatalf("summary row = %+v (%v)", byID["sp-gen"], genData)
	}
	if fnRow := byID["sp-fn"]; !fnRow.PayloadOmitted || fnRow.Data != "{}" {
		t.Fatalf("function summary = %+v, want an emptied payload marked omitted", fnRow)
	}
	// Only the payload fields go; the rest of the row's data stays, and a row
	// that had any payload field is marked, whatever its type.
	// Compared as JSON values: the PostgreSQL branch rewrites through jsonb,
	// which spaces and reorders keys.
	if agentRow := byID["sp-agent"]; !agentRow.PayloadOmitted || !sameJSON(agentRow.Data, `{"agent_name":"coder"}`) {
		t.Fatalf("agent summary = %+v, want handoffs out, agent_name kept, marked", agentRow)
	}
	// Nothing bulky: nothing removed, and not marked — the client would fetch
	// for nothing.
	if row := byID["sp-compact"]; row.PayloadOmitted || !sameJSON(row.Data, `{"before_items":3,"after_items":1}`) {
		t.Fatalf("compaction summary = %+v, want it untouched and unmarked", row)
	}
	if legacyRow := byID["sp-legacy"]; legacyRow.PayloadOmitted || legacyRow.Data != "not json" {
		t.Fatalf("legacy row = %+v, want it untouched", legacyRow)
	}

	// The full listing is whole again — as a JSON value: the rebuilt document
	// orders its keys.
	full, err := ts.ListBySession(ctx, id("s1"), "", 0)
	if err != nil || len(full) != 5 || !sameJSON(full[0].Data, genDoc) || full[0].PayloadOmitted {
		t.Fatalf("full listing = %+v (%v)", full, err)
	}
	if full[1].Data == "{}" || full[4].Data != "not json" {
		t.Fatalf("full listing payloads = %q / %q", full[1].Data, full[4].Data)
	}

	// One span, whole — scoped to the session, so another session's span of
	// the same id is not it.
	got, err := ts.GetBySpan(ctx, id("s1"), "sp-gen")
	if err != nil || !sameJSON(got.Data, genDoc) || got.PayloadOmitted {
		t.Fatalf("GetBySpan = %+v (%v)", got, err)
	}
	if _, err := ts.GetBySpan(ctx, id("s1"), "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing span: %v, want ErrNotFound", err)
	}
	if _, err := ts.GetBySpan(ctx, NewID(), "sp-gen"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another session's span: %v, want ErrNotFound", err)
	}
}

// sameJSON reports whether two documents hold the same value.
func sameJSON(a, b string) bool {
	var va, vb any
	if json.Unmarshal([]byte(a), &va) != nil || json.Unmarshal([]byte(b), &vb) != nil {
		return a == b
	}
	return reflect.DeepEqual(va, vb)
}
