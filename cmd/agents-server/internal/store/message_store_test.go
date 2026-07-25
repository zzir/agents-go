package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/zzir/agents-go/agents"
)

// contentOf unmarshals the item JSON and returns its "content" value.
func contentOf(t *testing.T, raw []byte) any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal %s: %v", raw, err)
	}
	return m["content"]
}

func TestNormalizeItemJSONWrapsStringContent(t *testing.T) {
	for _, role := range []string{"user", "system", "developer"} {
		raw := NormalizeItemJSON([]byte(`{"role":"` + role + `","content":"hello"}`))
		parts, ok := contentOf(t, raw).([]any)
		if !ok || len(parts) != 1 {
			t.Fatalf("role %s: content not a one-part array: %s", role, raw)
		}
		part := parts[0].(map[string]any)
		if part["type"] != "input_text" || part["text"] != "hello" {
			t.Fatalf("role %s: unexpected part: %s", role, raw)
		}
	}
}

func TestNormalizeItemJSONDropsNullContent(t *testing.T) {
	raw := NormalizeItemJSON([]byte(`{"type":"reasoning","id":"rs_1","summary":[],"content":null}`))
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := m["content"]; ok {
		t.Fatalf("null content not dropped: %s", raw)
	}
}

func TestNormalizeItemJSONLeavesOtherShapesAlone(t *testing.T) {
	for _, in := range []string{
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}`,
		`{"role":"assistant","content":"hi"}`,
		`{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"}`,
		`{"type":"function_call_output","call_id":"c1","output":"ok"}`,
		`not json`,
	} {
		if got := string(NormalizeItemJSON([]byte(in))); got != in {
			t.Fatalf("changed %q -> %q", in, got)
		}
	}
}

func TestNewItemMessageRawNormalizesAndProjects(t *testing.T) {
	m := NewItemMessageRaw("s1", "r1", "gpt-x", []byte(`{"role":"user","content":"hello"}`))
	if m.Kind != MessageKindItem || m.Role != "user" || m.Content != "hello" || m.SourceModel != "gpt-x" {
		t.Fatalf("unexpected projection: %+v", m)
	}
	parts, ok := contentOf(t, []byte(m.Item)).([]any)
	if !ok || len(parts) != 1 {
		t.Fatalf("item content not normalized to array at write time: %s", m.Item)
	}
}

func TestDeriveDisplay(t *testing.T) {
	call := NewItemMessageRaw("s1", "r1", "", []byte(`{"type":"function_call","id":"fc_1","call_id":"c1","name":"get_weather","arguments":"{\"city\":\"sf\"}"}`))
	var d map[string]string
	if err := json.Unmarshal(call.Display, &d); err != nil {
		t.Fatalf("unmarshal display: %v (%s)", err, call.Display)
	}
	if d["call_id"] != "c1" || d["name"] != "get_weather" || d["arguments"] != `{"city":"sf"}` {
		t.Fatalf("unexpected call display: %v", d)
	}

	out := NewItemMessageRaw("s1", "r1", "", []byte(`{"type":"function_call_output","call_id":"c1","output":"sunny"}`))
	if err := json.Unmarshal(out.Display, &d); err != nil {
		t.Fatalf("unmarshal output display: %v", err)
	}
	if d["call_id"] != "c1" || d["output"] != "sunny" {
		t.Fatalf("unexpected output display: %v", d)
	}

	text := NewItemMessageRaw("s1", "r1", "", []byte(`{"role":"user","content":"hi"}`))
	if text.Display != nil {
		t.Fatalf("message rows should have no display: %s", text.Display)
	}
}

func TestAdaptForeignItemJSON(t *testing.T) {
	if got := adaptForeignItemJSON([]byte(`{"type":"reasoning","id":"rs_1","summary":[]}`)); got != nil {
		t.Fatalf("reasoning item not dropped: %s", got)
	}
	got := adaptForeignItemJSON([]byte(`{"type":"message","role":"assistant","id":"msg_1","content":[{"type":"output_text","text":"hi"}],"status":"completed"}`))
	var m map[string]any
	if err := json.Unmarshal(got, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := m["id"]; ok {
		t.Fatalf("id not stripped: %s", got)
	}
	// Stripping must survive the SDK round-trip without resurrecting an empty id.
	item, err := agents.UnmarshalInputItem(got)
	if err != nil {
		t.Fatalf("unmarshal stripped item: %v", err)
	}
	out, err := agents.MarshalInputItem(item)
	if err != nil {
		t.Fatalf("marshal stripped item: %v", err)
	}
	var m2 map[string]any
	_ = json.Unmarshal(out, &m2)
	if _, ok := m2["id"]; ok {
		t.Fatalf("id resurrected on round-trip: %s", out)
	}
}

// End-to-end through sqlite: annotations never replay, and switching the run
// model drops foreign reasoning items and strips foreign ids while items from
// the same model replay untouched.
func TestSessionAdapterReplayPolicy(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	sa := NewSessionAdapter(db, "s1")
	sa.SetRunID("r1")
	sa.SetModel("model-a")

	rows := []Message{
		NewItemMessageRaw("s1", "r1", "model-a", []byte(`{"role":"user","content":"hi"}`)),
		NewItemMessageRaw("s1", "r1", "model-a", []byte(`{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"think"}]}`)),
		NewItemMessageRaw("s1", "r1", "model-a", []byte(`{"type":"message","role":"assistant","id":"msg_1","content":[{"type":"output_text","text":"hello"}],"status":"completed"}`)),
		NewAnnotationMessage("s1", "r1", "error", "boom"),
	}
	if _, err := db.NewInsert().Model(&rows).Exec(ctx); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Same model: everything except the annotation replays, ids intact.
	items, err := agents.SessionItems(ctx, sa, 0)
	if err != nil {
		t.Fatalf("get items: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("same model: want 3 items, got %d", len(items))
	}

	// Different model: reasoning dropped, assistant id stripped.
	sa.SetModel("model-b")
	items, err = agents.SessionItems(ctx, sa, 0)
	if err != nil {
		t.Fatalf("get items: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("foreign model: want 2 items (reasoning dropped), got %d", len(items))
	}
	for _, it := range items {
		raw, err := agents.MarshalInputItem(it)
		if err != nil {
			t.Fatalf("marshal replayed item: %v", err)
		}
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		if _, ok := m["id"]; ok {
			t.Fatalf("foreign id not stripped: %s", raw)
		}
	}
}

func TestGetMessagesBackfillsDisplay(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	// A legacy row: item JSON present but no display column.
	legacy := Message{
		SessionID: "s1", RunID: "r1", Role: "tool_call", Content: "f({})",
		Item:      `{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"}`,
		CreatedAt: time.Now().UTC(),
	}
	if _, err := db.NewInsert().Model(&legacy).Exec(ctx); err != nil {
		t.Fatalf("insert: %v", err)
	}
	msgs, err := NewMessageStore(db).GetMessages(ctx, "s1", 0, 0)
	if err != nil {
		t.Fatalf("get messages: %v", err)
	}
	if len(msgs) != 1 || len(msgs[0].Display) == 0 {
		t.Fatalf("display not backfilled: %+v", msgs)
	}
	var d map[string]string
	if err := json.Unmarshal(msgs[0].Display, &d); err != nil || d["name"] != "f" {
		t.Fatalf("bad backfilled display: %s", msgs[0].Display)
	}
}

// After a compaction the summary row is appended last (highest id), but the
// model must see it first — a summary describes the older history. GetItems
// must front-load role=compaction rows, matching SlidingWindowSession's
// [summary, kept...] ordering.
func TestGetItemsFrontLoadsCompactionSummary(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	sa := NewSessionAdapter(db, "s1")

	compacted := NewItemMessageRaw("s1", "r1", "m", []byte(`{"role":"user","content":"old question"}`))
	compacted.Compacted = true
	kept1 := NewItemMessageRaw("s1", "r1", "m", []byte(`{"type":"function_call_output","call_id":"c1","output":"kept output"}`))
	kept2 := NewItemMessageRaw("s1", "r1", "m", []byte(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"final"}],"status":"completed"}`))
	summary, err := NewItemMessage("s1", "r1", "m", responsesSummaryItem(t, "summary of older history"))
	if err != nil {
		t.Fatalf("summary item: %v", err)
	}
	summary.Role = "compaction"

	rows := []Message{compacted, kept1, kept2, summary}
	if _, err := db.NewInsert().Model(&rows).Exec(ctx); err != nil {
		t.Fatalf("insert: %v", err)
	}

	items, err := agents.SessionItems(ctx, sa, 0)
	if err != nil {
		t.Fatalf("get items: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("want 3 items (compacted row excluded), got %d", len(items))
	}
	first, err := agents.MarshalInputItem(items[0])
	if err != nil {
		t.Fatalf("marshal first item: %v", err)
	}
	if !json.Valid(first) || !bytesContains(first, "summary of older history") {
		t.Fatalf("summary not front-loaded, first item: %s", first)
	}
	last, _ := agents.MarshalInputItem(items[2])
	if !bytesContains(last, "final") {
		t.Fatalf("kept items reordered, last item: %s", last)
	}
}

func responsesSummaryItem(t *testing.T, text string) agents.TResponseInputItem {
	t.Helper()
	item, err := agents.UnmarshalInputItem([]byte(`{"role":"system","content":"` + text + `"}`))
	if err != nil {
		t.Fatalf("build summary item: %v", err)
	}
	return item
}

func bytesContains(b []byte, s string) bool {
	return json.Valid(b) && string(b) != "" && strings.Contains(string(b), s)
}

// Some Responses-compatible backends (vLLM and friends) emit message content
// parts typed "text" instead of output_text; the projection must not come out
// empty for them.
func TestExtractTextContentTextPartType(t *testing.T) {
	m := NewItemMessageRaw("s1", "r1", "qwen", []byte(`{"type":"message","role":"assistant","content":[{"type":"text","text":"最终回答"}],"status":"completed"}`))
	if m.Role != "assistant" || m.Content != "最终回答" {
		t.Fatalf("text-part projection failed: role=%q content=%q", m.Role, m.Content)
	}
}

// Rows persisted with an empty content projection (written before the
// extractor understood their item shape) heal on read.
func TestGetMessagesBackfillsContent(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	legacy := Message{
		SessionID: "s1", RunID: "r1", Kind: MessageKindItem, Role: "assistant", Content: "",
		Item:      `{"type":"message","role":"assistant","content":[{"type":"text","text":"healed"}],"status":"completed"}`,
		CreatedAt: time.Now().UTC(),
	}
	annotation := NewAnnotationMessage("s1", "r1", "error", "")
	rows := []Message{legacy, annotation}
	if _, err := db.NewInsert().Model(&rows).Exec(ctx); err != nil {
		t.Fatalf("insert: %v", err)
	}
	msgs, err := NewMessageStore(db).GetMessages(ctx, "s1", 0, 0)
	if err != nil {
		t.Fatalf("get messages: %v", err)
	}
	if len(msgs) != 2 || msgs[0].Content != "healed" {
		t.Fatalf("content not backfilled: %+v", msgs)
	}
	if msgs[1].Content != "" {
		t.Fatalf("annotation content should stay empty: %+v", msgs[1])
	}
}

// The normalized shape must survive the SDK round-trip the runner performs:
// UnmarshalInputItem when loading history, MarshalInputItem when sending the
// request — with content still an array and the text intact.
func TestNormalizeItemJSONRoundTrip(t *testing.T) {
	norm := NormalizeItemJSON([]byte(`{"role":"user","content":"hello world"}`))
	item, err := agents.UnmarshalInputItem(norm)
	if err != nil {
		t.Fatalf("unmarshal normalized item: %v", err)
	}
	out, err := agents.MarshalInputItem(item)
	if err != nil {
		t.Fatalf("marshal round-tripped item: %v", err)
	}
	parts, ok := contentOf(t, out).([]any)
	if !ok || len(parts) != 1 {
		t.Fatalf("round-trip lost array content: %s", out)
	}
	part := parts[0].(map[string]any)
	if part["type"] != "input_text" || part["text"] != "hello world" {
		t.Fatalf("round-trip lost text: %s", out)
	}
}
