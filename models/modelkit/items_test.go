package modelkit

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/zzir/agents-go/agents"
)

func TestMessageItemRoundTrips(t *testing.T) {
	item, err := MessageItem("msg_1", "hello")
	if err != nil {
		t.Fatal(err)
	}
	if item.Type != "message" {
		t.Fatalf("type = %q, want message", item.Type)
	}
	if item.RawJSON() == "" {
		t.Fatal("RawJSON is empty")
	}
	msg := item.AsMessage()
	var text strings.Builder
	for _, part := range msg.Content {
		text.WriteString(part.AsOutputText().Text)
	}
	if text.String() != "hello" {
		t.Fatalf("text = %q, want hello", text.String())
	}
	// The property everything downstream depends on: the item converts back
	// into model input for the next turn.
	in, err := agents.OutputToInput([]agents.OutputItem{item})
	if err != nil {
		t.Fatal(err)
	}
	if len(in) != 1 || in[0].OfOutputMessage == nil {
		t.Fatalf("expected an output-message input item, got %+v", in)
	}
}

func TestFunctionCallItem(t *testing.T) {
	item, err := FunctionCallItem("fc_1", "call_1", "get_weather", `{"city":"Oslo"}`)
	if err != nil {
		t.Fatal(err)
	}
	fc := item.AsFunctionCall()
	if fc.CallID != "call_1" || fc.Name != "get_weather" || fc.Arguments != `{"city":"Oslo"}` {
		t.Fatalf("unexpected function call: %+v", fc)
	}
	if _, err := agents.OutputToInput([]agents.OutputItem{item}); err != nil {
		t.Fatal(err)
	}
}

func TestFunctionCallItemEmptyArguments(t *testing.T) {
	item, err := FunctionCallItem("fc_1", "call_1", "noop", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := item.AsFunctionCall().Arguments; got != "{}" {
		t.Fatalf("arguments = %q, want {}", got)
	}
}

func TestReasoningItemTextAndEncrypted(t *testing.T) {
	item, err := ReasoningItem("rs_1", "thinking...", "sig-abc")
	if err != nil {
		t.Fatal(err)
	}
	r := item.AsReasoning()
	if len(r.Content) != 1 || r.Content[0].Text != "thinking..." {
		t.Fatalf("content = %+v, want one reasoning_text part", r.Content)
	}
	if r.EncryptedContent != "sig-abc" {
		t.Fatalf("encrypted_content = %q", r.EncryptedContent)
	}
	// The continuity blob must survive conversion back to input — it is the
	// only thing that lets a signature-carrying backend resume its reasoning.
	in, err := agents.OutputToInput([]agents.OutputItem{item})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := agents.MarshalInputItem(in[0])
	if err != nil {
		t.Fatal(err)
	}
	var probe struct {
		EncryptedContent string `json:"encrypted_content"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatal(err)
	}
	if probe.EncryptedContent != "sig-abc" {
		t.Fatalf("encrypted_content after round trip = %q, want sig-abc", probe.EncryptedContent)
	}
}

func TestReasoningItemEncryptedOnly(t *testing.T) {
	item, err := ReasoningItem("rs_1", "", "opaque")
	if err != nil {
		t.Fatal(err)
	}
	r := item.AsReasoning()
	if len(r.Content) != 0 {
		t.Fatalf("content = %+v, want empty", r.Content)
	}
	if r.EncryptedContent != "opaque" {
		t.Fatalf("encrypted_content = %q", r.EncryptedContent)
	}
}
