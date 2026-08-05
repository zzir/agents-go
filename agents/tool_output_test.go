package agents

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/zzir/agents-go/agents/session"
)

// marshalRaw renders a tool-result item's wire form for inspection.
func marshalRaw(t *testing.T, item *RunItem) map[string]any {
	t.Helper()
	b, err := session.MarshalInputItem(*item.RawInput)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return m
}

func TestFunctionCallOutputString(t *testing.T) {
	item := newFunctionCallOutputItem(&Agent{Name: "a"}, "call_1", map[string]int{"n": 2})
	m := marshalRaw(t, item)
	if m["type"] != "function_call_output" {
		t.Fatalf("type = %v", m["type"])
	}
	// Plain values stringify to a JSON string, not a content list.
	out, ok := m["output"].(string)
	if !ok {
		t.Fatalf("output is not a string: %T", m["output"])
	}
	if !strings.Contains(out, `"n":2`) {
		t.Errorf("output = %q", out)
	}
}

func TestFunctionCallOutputImage(t *testing.T) {
	img := ToolOutputImageFromBytes("image/png", []byte{0x89, 0x50, 0x4e, 0x47})
	item := newFunctionCallOutputItem(&Agent{Name: "a"}, "call_1", img)
	m := marshalRaw(t, item)
	list, ok := m["output"].([]any)
	if !ok {
		t.Fatalf("output is not a content list: %T", m["output"])
	}
	if len(list) != 1 {
		t.Fatalf("len(output) = %d", len(list))
	}
	part := list[0].(map[string]any)
	if part["type"] != "input_image" {
		t.Errorf("part type = %v", part["type"])
	}
	url, _ := part["image_url"].(string)
	if !strings.HasPrefix(url, "data:image/png;base64,") {
		t.Errorf("image_url = %q", url)
	}
}

func TestFunctionCallOutputMixed(t *testing.T) {
	parts := []ToolOutputContent{
		ToolOutputText{Text: "here is the chart"},
		ToolOutputImage{ImageURL: "https://example.com/c.png", Detail: "high"},
		ToolOutputFile{FileURL: "https://example.com/r.pdf", Filename: "r.pdf"},
	}
	item := newFunctionCallOutputItem(&Agent{Name: "a"}, "call_1", parts)
	m := marshalRaw(t, item)
	list, ok := m["output"].([]any)
	if !ok || len(list) != 3 {
		t.Fatalf("output = %v", m["output"])
	}
	types := []string{}
	for _, p := range list {
		types = append(types, p.(map[string]any)["type"].(string))
	}
	want := "input_text,input_image,input_file"
	if got := strings.Join(types, ","); got != want {
		t.Errorf("part types = %q, want %q", got, want)
	}
	// Detail and filename should survive.
	if d := list[1].(map[string]any)["detail"]; d != "high" {
		t.Errorf("detail = %v", d)
	}
	if fn := list[2].(map[string]any)["filename"]; fn != "r.pdf" {
		t.Errorf("filename = %v", fn)
	}
}

func TestFunctionCallOutputContentRoundTrip(t *testing.T) {
	img := ToolOutputImageFromBytes("image/png", []byte{1, 2, 3})
	item := newFunctionCallOutputItem(&Agent{Name: "a"}, "call_1", img)
	b, err := session.MarshalInputItem(*item.RawInput)
	if err != nil {
		t.Fatal(err)
	}
	back, err := session.UnmarshalInputItem(b)
	if err != nil {
		t.Fatalf("round-trip: %v", err)
	}
	if back.OfFunctionCallOutput == nil {
		t.Fatalf("decoded item is not a function_call_output: %+v", back)
	}
	if arr := back.OfFunctionCallOutput.Output.OfResponseFunctionCallOutputItemArray; len(arr) != 1 {
		t.Fatalf("content list lost on round-trip: %+v", back.OfFunctionCallOutput.Output)
	}
}

// An empty []ToolOutputContent must not produce a content list (it would be an
// invalid empty output); it falls back to the string path.
func TestEmptyToolOutputContentFallsBack(t *testing.T) {
	item := newFunctionCallOutputItem(&Agent{Name: "a"}, "call_1", []ToolOutputContent{})
	m := marshalRaw(t, item)
	if _, ok := m["output"].(string); !ok {
		t.Errorf("empty content list should fall back to string output, got %T", m["output"])
	}
}
