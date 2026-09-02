package agents

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/zzir/agents-go/agents/session"
)

// An assistant message's output_text survives a JSON round-trip through
// session.UnmarshalInputItem: a plain union decode matches
// EasyInputMessageParam first and silently drops the content.
func TestUnmarshalInputItem_PreservesAssistantContent(t *testing.T) {
	in, err := outputItemToInput(messageOutput(t, "hello from assistant"))
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	got, err := session.UnmarshalInputItem(data)
	if err != nil {
		t.Fatal(err)
	}
	if got.OfOutputMessage == nil {
		t.Fatalf("assistant message did not decode as OfOutputMessage (content dropped)")
	}
	re, err := session.MarshalInputItem(got)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(re), "output_text") || !strings.Contains(string(re), "hello from assistant") {
		t.Errorf("round-tripped item lost content: %s", re)
	}
}

// An "easy" role message serialized without a "type" discriminator still
// decodes.
func TestUnmarshalInputItem_EasyMessageWithoutType(t *testing.T) {
	got, err := session.UnmarshalInputItem([]byte(`{"role":"user","content":"hi there"}`))
	if err != nil {
		t.Fatalf("UnmarshalInputItem failed on type-less easy message: %v", err)
	}
	if got.OfMessage == nil {
		t.Fatalf("type-less easy message did not decode as OfMessage")
	}
}
