package agents

import (
	"strings"
	"testing"
)

// TestUnmarshalItems_PreservesAssistantContent covers audit: UnmarshalItems
// must route each element through UnmarshalInputItem so an assistant message's
// output_text survives a MarshalItems -> UnmarshalItems round-trip. A plain slice
// decode matches EasyInputMessageParam first, silently dropping the content —
// external Session backends that store via this pair would lose assistant text.
func TestUnmarshalItems_PreservesAssistantContent(t *testing.T) {
	in, err := outputItemToInput(messageOutput(t, "hello from assistant"))
	if err != nil {
		t.Fatal(err)
	}
	data, err := MarshalItems([]TResponseInputItem{in})
	if err != nil {
		t.Fatal(err)
	}
	got, err := UnmarshalItems(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d items, want 1", len(got))
	}
	if got[0].OfOutputMessage == nil {
		t.Fatalf("assistant message did not decode as OfOutputMessage (content dropped)")
	}
	re, err := MarshalInputItem(got[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(re), "output_text") || !strings.Contains(string(re), "hello from assistant") {
		t.Errorf("round-tripped item lost content: %s", re)
	}
}

// TestUnmarshalItems_EasyMessageWithoutType covers audit: an "easy" role
// message serialized without a "type" discriminator must still decode. A plain
// slice decode cannot detect it (the union has no discriminator to match).
func TestUnmarshalItems_EasyMessageWithoutType(t *testing.T) {
	got, err := UnmarshalItems([]byte(`[{"role":"user","content":"hi there"}]`))
	if err != nil {
		t.Fatalf("UnmarshalItems failed on type-less easy message: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d items, want 1", len(got))
	}
	if got[0].OfMessage == nil {
		t.Fatalf("type-less easy message did not decode as OfMessage")
	}
}

// TestUnmarshalItems_EmptyInputs preserves the nil-tolerant contract.
func TestUnmarshalItems_EmptyInputs(t *testing.T) {
	for _, data := range [][]byte{nil, []byte(""), []byte("null")} {
		got, err := UnmarshalItems(data)
		if err != nil {
			t.Errorf("UnmarshalItems(%q) error: %v", data, err)
		}
		if got != nil {
			t.Errorf("UnmarshalItems(%q) = %v, want nil", data, got)
		}
	}
}
