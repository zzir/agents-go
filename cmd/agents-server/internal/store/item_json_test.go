package store

import (
	"encoding/json"
	"testing"

	"github.com/zzir/agents-go/agents"
)

// contentJSON unmarshals item JSON and returns its "content" value.
func contentJSON(t *testing.T, raw []byte) any {
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
		parts, ok := contentJSON(t, raw).([]any)
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
	parts, ok := contentJSON(t, out).([]any)
	if !ok || len(parts) != 1 {
		t.Fatalf("round-trip lost array content: %s", out)
	}
	part := parts[0].(map[string]any)
	if part["type"] != "input_text" || part["text"] != "hello world" {
		t.Fatalf("round-trip lost text: %s", out)
	}
}
