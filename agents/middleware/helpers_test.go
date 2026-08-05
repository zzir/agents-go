package middleware

import (
	"context"
	"encoding/json"
	"iter"
	"testing"

	"github.com/openai/openai-go/v3/responses"
	"github.com/zzir/agents-go/agents"
)

// scriptedModel replays queued responses. Once they run out it answers with an
// empty response, which ends a run rather than hanging it.
type scriptedModel struct {
	responses []*agents.ModelResponse
	idx       int
	calls     int
}

func (m *scriptedModel) next() *agents.ModelResponse {
	m.calls++
	if m.idx >= len(m.responses) {
		return &agents.ModelResponse{Usage: agents.NewUsage()}
	}
	resp := m.responses[m.idx]
	m.idx++
	if resp.Usage == nil {
		resp.Usage = agents.NewUsage()
	}
	return resp
}

func (m *scriptedModel) Respond(context.Context, agents.ModelRequest) (*agents.ModelResponse, error) {
	return m.next(), nil
}

func (m *scriptedModel) StreamResponse(context.Context, agents.ModelRequest) iter.Seq2[*agents.ResponseStreamEvent, error] {
	return func(yield func(*agents.ResponseStreamEvent, error) bool) {
		resp := m.next()
		raw := make([]json.RawMessage, 0, len(resp.Output))
		for i := range resp.Output {
			raw = append(raw, json.RawMessage(resp.Output[i].RawJSON()))
		}
		out, _ := json.Marshal(raw)
		payload := `{"type":"response.completed","sequence_number":0,"response":{"id":"resp_1","output":` +
			string(out) + `,"usage":{"input_tokens":5,"output_tokens":3,"total_tokens":8,` +
			`"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":0}}}}`
		var event agents.ResponseStreamEvent
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			panic(err)
		}
		yield(&event, nil)
	}
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func outputItem(t *testing.T, raw string) agents.OutputItem {
	t.Helper()
	var item responses.ResponseOutputItemUnion
	if err := json.Unmarshal([]byte(raw), &item); err != nil {
		t.Fatalf("build output item: %v", err)
	}
	return item
}

func message(t *testing.T, text string) agents.OutputItem {
	t.Helper()
	return outputItem(t, `{"type":"message","id":"msg_1","status":"completed","role":"assistant",`+
		`"content":[{"type":"output_text","text":`+quote(text)+`,"annotations":[]}]}`)
}

func toolCall(t *testing.T, name, callID string) agents.OutputItem {
	t.Helper()
	return outputItem(t, `{"type":"function_call","id":"fc_1","call_id":`+quote(callID)+
		`,"name":`+quote(name)+`,"arguments":"{}","status":"completed"}`)
}

func resp(items ...agents.OutputItem) *agents.ModelResponse {
	return &agents.ModelResponse{Output: items, Usage: agents.NewUsage()}
}

// says builds an agent that answers with each queued text in turn.
func says(t *testing.T, texts ...string) *agents.Agent {
	t.Helper()
	out := make([]*agents.ModelResponse, 0, len(texts))
	for _, s := range texts {
		out = append(out, resp(message(t, s)))
	}
	return &agents.Agent{Name: "a", ModelImpl: &scriptedModel{responses: out}}
}
