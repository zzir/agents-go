package bridge

import (
	"encoding/json"
	"testing"

	"github.com/openai/openai-go/v3/responses"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
)

func messageItem(t *testing.T, contentJSON string) *agents.MessageOutputItem {
	t.Helper()
	var item responses.ResponseOutputItemUnion
	raw := `{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":` + contentJSON + `}`
	if err := json.Unmarshal([]byte(raw), &item); err != nil {
		t.Fatal(err)
	}
	return &agents.MessageOutputItem{Raw: item}
}

// handleStreamEvent must bridge message_output_created into a run.message
// event carrying the turn's full text: interim messages between tool calls
// have no other authoritative live signal — run.step deltas may be absent on
// some backends, and a resumed segment has none for its earlier turns.
func TestHandleStreamEvent_MessageOutputCreated(t *testing.T) {
	ev := &agents.RunItemStreamEvent{
		Name: "message_output_created",
		Item: messageItem(t, `[{"type":"output_text","text":"writing the file","annotations":[]}]`),
	}

	var gotType string
	var gotPayload any
	(&Runner{}).handleStreamEvent(ev, "run_1", func(typ string, payload any) {
		gotType = typ
		gotPayload = payload
	})

	if gotType != "run.message" {
		t.Fatalf("event type = %q, want run.message", gotType)
	}
	msg, ok := gotPayload.(protocol.RunMessage)
	if !ok {
		t.Fatalf("payload type = %T, want protocol.RunMessage", gotPayload)
	}
	if msg.RunID != "run_1" || msg.Text != "writing the file" {
		t.Errorf("payload = %+v, want run_1 / %q", msg, "writing the file")
	}
}

// A message with no text (e.g. refusal-only content) must not produce an
// empty chat bubble.
func TestHandleStreamEvent_EmptyMessageSkipped(t *testing.T) {
	ev := &agents.RunItemStreamEvent{
		Name: "message_output_created",
		Item: messageItem(t, `[]`),
	}

	called := false
	(&Runner{}).handleStreamEvent(ev, "run_1", func(string, any) { called = true })
	if called {
		t.Error("expected no event for an empty message")
	}
}

func reasoningItem(t *testing.T, bodyJSON string) *agents.ReasoningItem {
	t.Helper()
	var item responses.ResponseOutputItemUnion
	raw := `{"type":"reasoning","id":"rs_1",` + bodyJSON + `}`
	if err := json.Unmarshal([]byte(raw), &item); err != nil {
		t.Fatal(err)
	}
	return &agents.ReasoningItem{Raw: item}
}

// handleStreamEvent must bridge reasoning_item_created into run.reasoning_item
// with the turn's full thinking text — for backends that use the standard
// summary array and for those that put raw reasoning in content parts.
func TestHandleStreamEvent_ReasoningItemCreated(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"summary", `"summary":[{"type":"summary_text","text":"think hard"}]`, "think hard"},
		{"content fallback", `"summary":[],"content":[{"type":"reasoning_text","text":"raw thoughts"}]`, "raw thoughts"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := &agents.RunItemStreamEvent{
				Name: "reasoning_item_created",
				Item: reasoningItem(t, tc.body),
			}
			var gotType string
			var gotPayload any
			(&Runner{}).handleStreamEvent(ev, "run_1", func(typ string, payload any) {
				gotType = typ
				gotPayload = payload
			})
			if gotType != "run.reasoning_item" {
				t.Fatalf("event type = %q, want run.reasoning_item", gotType)
			}
			ri, ok := gotPayload.(protocol.RunReasoningItem)
			if !ok {
				t.Fatalf("payload type = %T, want protocol.RunReasoningItem", gotPayload)
			}
			if ri.RunID != "run_1" || ri.Text != tc.want {
				t.Errorf("payload = %+v, want run_1 / %q", ri, tc.want)
			}
		})
	}
}

// Encrypted-only reasoning (no summary, no content text) must not emit an
// empty thinking block.
func TestHandleStreamEvent_EmptyReasoningSkipped(t *testing.T) {
	ev := &agents.RunItemStreamEvent{
		Name: "reasoning_item_created",
		Item: reasoningItem(t, `"summary":[]`),
	}

	called := false
	(&Runner{}).handleStreamEvent(ev, "run_1", func(string, any) { called = true })
	if called {
		t.Error("expected no event for empty reasoning")
	}
}
