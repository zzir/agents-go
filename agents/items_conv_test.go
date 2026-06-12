package agents

import (
	"encoding/json"
	"testing"

	"github.com/openai/openai-go/v3/responses"
)

func TestOutputToInputRoundTrip(t *testing.T) {
	// Simulate a model output: a function call item.
	raw := `{"type":"function_call","id":"fc_1","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"SF\"}","status":"completed"}`
	var item responses.ResponseOutputItemUnion
	if err := json.Unmarshal([]byte(raw), &item); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if item.RawJSON() == "" {
		t.Fatalf("RawJSON empty after unmarshal")
	}
	in, err := OutputToInput([]TResponseOutputItem{item})
	if err != nil {
		t.Fatalf("OutputToInput: %v", err)
	}
	if len(in) != 1 {
		t.Fatalf("len = %d", len(in))
	}
	if in[0].OfFunctionCall == nil {
		t.Fatalf("expected OfFunctionCall, got %+v", in[0])
	}
	if in[0].OfFunctionCall.Name != "get_weather" {
		t.Errorf("name = %q", in[0].OfFunctionCall.Name)
	}
	// Re-marshal to confirm it is a valid input item.
	if _, err := json.Marshal(in[0]); err != nil {
		t.Errorf("marshal input item: %v", err)
	}
}

// Assistant messages are the one output type the input union decoder mangles
// (it matches EasyInputMessageParam first and drops output_text content), so
// the conversion must preserve content, id and status explicitly.
func TestOutputToInputAssistantMessage(t *testing.T) {
	raw := `{"type":"message","id":"msg_1","status":"completed","role":"assistant",` +
		`"content":[{"type":"output_text","text":"hello world","annotations":[]}]}`
	var item responses.ResponseOutputItemUnion
	if err := json.Unmarshal([]byte(raw), &item); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	in, err := outputItemToInput(item)
	if err != nil {
		t.Fatalf("outputItemToInput: %v", err)
	}
	if in.OfOutputMessage == nil {
		t.Fatalf("expected OfOutputMessage, got %+v", in)
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal input item: %v", err)
	}
	assertMessageJSON(t, b)

	// The stored form must survive UnmarshalInputItem (session reload).
	in2, err := UnmarshalInputItem(b)
	if err != nil {
		t.Fatalf("UnmarshalInputItem: %v", err)
	}
	if in2.OfOutputMessage == nil {
		t.Fatalf("expected OfOutputMessage after reload, got %+v", in2)
	}
	b2, err := json.Marshal(in2)
	if err != nil {
		t.Fatalf("re-marshal input item: %v", err)
	}
	assertMessageJSON(t, b2)
}

func assertMessageJSON(t *testing.T, b []byte) {
	t.Helper()
	var m struct {
		ID      string `json:"id"`
		Status  string `json:"status"`
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode message JSON %s: %v", b, err)
	}
	if m.Role != "assistant" || m.ID != "msg_1" || m.Status != "completed" {
		t.Errorf("lost fields: %s", b)
	}
	if len(m.Content) != 1 || m.Content[0].Type != "output_text" || m.Content[0].Text != "hello world" {
		t.Errorf("lost content: %s", b)
	}
}

func TestUnmarshalInputItemRejectsGarbage(t *testing.T) {
	if _, err := UnmarshalInputItem([]byte(`{"bogus":true}`)); err == nil {
		t.Fatal("expected error for unrecognized item shape")
	}
	// Easy messages (no "type" discriminator) must still decode.
	in, err := UnmarshalInputItem([]byte(`{"role":"user","content":"hi"}`))
	if err != nil {
		t.Fatalf("easy message: %v", err)
	}
	if in.OfMessage == nil {
		t.Fatalf("expected OfMessage, got %+v", in)
	}
}
