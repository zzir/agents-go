package agents

import (
	"context"
	"iter"
	"testing"

	"github.com/zzir/agents-go/agents/session"
)

// recordingModel captures every ModelRequest it receives.
type recordingModel struct {
	responses []*ModelResponse
	idx       int
	reqs      []ModelRequest
}

func (m *recordingModel) Respond(_ context.Context, req ModelRequest) (*ModelResponse, error) {
	m.reqs = append(m.reqs, req)
	var resp *ModelResponse
	if m.idx < len(m.responses) {
		resp = m.responses[m.idx]
		m.idx++
	} else {
		resp = &ModelResponse{Usage: NewUsage()}
	}
	if resp.Usage == nil {
		resp.Usage = NewUsage()
	}
	return resp, nil
}

func (m *recordingModel) StreamResponse(context.Context, ModelRequest) iter.Seq2[*ResponseStreamEvent, error] {
	return func(func(*ResponseStreamEvent, error) bool) {}
}

func TestConversationIDSendsIncrementalInput(t *testing.T) {
	model := &recordingModel{responses: []*ModelResponse{
		{Output: []OutputItem{messageOutput(t, "hi")}, Usage: NewUsage(), ResponseID: "resp_1"},
	}}
	agent := &Agent{Name: "a", Model: "m"}

	_, err := RunSync(context.Background(), agent, "hello", RunOptions{Conversation: ConversationOptions{ConversationID: "conv_abc"}, Model: ModelOptions{Override: model}})
	if err != nil {
		t.Fatal(err)
	}
	if len(model.reqs) != 1 {
		t.Fatalf("calls = %d", len(model.reqs))
	}
	if model.reqs[0].ConversationID != "conv_abc" {
		t.Errorf("ConversationID = %q", model.reqs[0].ConversationID)
	}
	if len(model.reqs[0].Input) != 1 {
		t.Fatalf("turn-1 input len = %d, want 1", len(model.reqs[0].Input))
	}
}

func TestConversationIDIncrementalAcrossToolTurn(t *testing.T) {
	// Turn 1: model calls a tool. Turn 2: model produces final text.
	model := &recordingModel{responses: []*ModelResponse{
		{Output: []OutputItem{functionCallOutput(t, "echo", "call_1", "{}")}, Usage: NewUsage(), ResponseID: "resp_1"},
		{Output: []OutputItem{messageOutput(t, "done")}, Usage: NewUsage(), ResponseID: "resp_2"},
	}}
	echo := NewTool("echo", "echo", func(context.Context, *ToolContext, struct{}) (string, error) {
		return "ok", nil
	})
	agent := &Agent{Name: "a", Model: "m", Tools: []*Tool{echo}}

	_, err := RunSync(context.Background(), agent, "hello", RunOptions{Conversation: ConversationOptions{ConversationID: "conv_abc"}, Model: ModelOptions{Override: model}})
	if err != nil {
		t.Fatal(err)
	}
	if len(model.reqs) != 2 {
		t.Fatalf("calls = %d, want 2", len(model.reqs))
	}
	// Turn 2 must send only the new items (the tool output), not the whole
	// history, because the server already holds turn-1 items.
	if got := len(model.reqs[1].Input); got != 1 {
		t.Errorf("turn-2 input len = %d, want 1 (incremental tool output only)", got)
	}
	if model.reqs[1].ConversationID != "conv_abc" {
		t.Errorf("turn-2 ConversationID = %q", model.reqs[1].ConversationID)
	}
}

func TestConversationIDRejectsSession(t *testing.T) {
	agent := &Agent{Name: "a", Model: "m"}
	_, err := RunSync(context.Background(), agent, "hi", RunOptions{Conversation: ConversationOptions{ConversationID: "conv_abc", Session: session.NewInMemorySession()}, Model: ModelOptions{Override: &recordingModel{}}})
	if err == nil {
		t.Fatal("expected error combining ConversationID with a Session")
	}
}

// modelRespID is modelResp with an explicit response id, for chaining tests.
func modelRespID(id string, items ...OutputItem) *ModelResponse {
	r := modelResp(items...)
	r.ResponseID = id
	return r
}

// The previous_response_id mode sends only new items and chains the response ID.
func TestPreviousResponseID(t *testing.T) {
	tool := NewTool("noop", "", func(ctx context.Context, tc *ToolContext, a struct{}) (string, error) {
		return "ok", nil
	})
	model := &fakeModel{responses: []*ModelResponse{
		modelRespID("resp_1", functionCallOutput(t, "noop", "c1", `{}`)),
		modelRespID("resp_2", messageOutput(t, "done")),
	}}
	agent := &Agent{Name: "a", Tools: []*Tool{tool}, ModelImpl: model}

	res, err := RunSync(context.Background(), agent, "go", RunOptions{Conversation: ConversationOptions{UsePreviousResponseID: true}})
	if err != nil {
		t.Fatal(err)
	}
	if res.FinalOutputString() != "done" {
		t.Errorf("final = %q", res.FinalOutputString())
	}
	// Turn 2 must chain via previous_response_id and send only the new tool output.
	if model.lastReq.PreviousResponseID != "resp_1" {
		t.Errorf("previous_response_id = %q, want resp_1", model.lastReq.PreviousResponseID)
	}
	if len(model.lastReq.Input) != 1 {
		t.Errorf("turn-2 input = %d items, want 1 (only the new tool output)", len(model.lastReq.Input))
	}
}

func TestPreviousResponseID_Disabled(t *testing.T) {
	// Without the opt-in, full history is resent and no previous_response_id set.
	tool := NewTool("noop", "", func(ctx context.Context, tc *ToolContext, a struct{}) (string, error) {
		return "ok", nil
	})
	model := &fakeModel{responses: []*ModelResponse{
		modelRespID("resp_1", functionCallOutput(t, "noop", "c1", `{}`)),
		modelRespID("resp_2", messageOutput(t, "done")),
	}}
	agent := &Agent{Name: "a", Tools: []*Tool{tool}, ModelImpl: model}

	if _, err := RunSync(context.Background(), agent, "go", RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if model.lastReq.PreviousResponseID != "" {
		t.Errorf("previous_response_id should be empty when disabled, got %q", model.lastReq.PreviousResponseID)
	}
	// Full history: user msg + tool call + tool output = 3.
	if len(model.lastReq.Input) != 3 {
		t.Errorf("turn-2 input = %d items, want 3 (full history)", len(model.lastReq.Input))
	}
}

// Streaming loop also honors previous_response_id.
func TestPreviousResponseID_Streaming(t *testing.T) {
	tool := NewTool("noop", "", func(ctx context.Context, tc *ToolContext, a struct{}) (string, error) {
		return "ok", nil
	})
	model := &fakeModel{responses: []*ModelResponse{
		modelRespID("resp_1", functionCallOutput(t, "noop", "c1", `{}`)),
		modelRespID("resp_2", messageOutput(t, "done")),
	}}
	agent := &Agent{Name: "a", Tools: []*Tool{tool}, ModelImpl: model}

	stream, _ := Run(context.Background(), agent, "go", RunOptions{Conversation: ConversationOptions{UsePreviousResponseID: true}})
	_, res, err := streamRun(stream)
	if err != nil {
		t.Fatal(err)
	}
	if res.FinalOutputString() != "done" {
		t.Errorf("final = %q", res.FinalOutputString())
	}
	if model.lastReq.PreviousResponseID == "" {
		t.Error("streaming turn 2 should chain via previous_response_id")
	}
	if len(model.lastReq.Input) != 1 {
		t.Errorf("streaming turn-2 input = %d items, want 1 (only the new tool output)", len(model.lastReq.Input))
	}
}
