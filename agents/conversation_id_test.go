package agents

import (
	"context"
	"iter"
	"testing"
)

// recordingModel captures every ModelRequest it receives.
type recordingModel struct {
	responses []*ModelResponse
	idx       int
	reqs      []ModelRequest
}

func (m *recordingModel) GetResponse(_ context.Context, req ModelRequest) (*ModelResponse, error) {
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

func (m *recordingModel) StreamResponse(context.Context, ModelRequest) iter.Seq2[*TResponseStreamEvent, error] {
	return func(func(*TResponseStreamEvent, error) bool) {}
}

func TestConversationIDSendsIncrementalInput(t *testing.T) {
	model := &recordingModel{responses: []*ModelResponse{
		{Output: []TResponseOutputItem{messageOutput(t, "hi")}, Usage: NewUsage(), ResponseID: "resp_1"},
	}}
	agent := &Agent{Name: "a", Model: "m"}

	_, err := Run(context.Background(), agent, "hello", RunOptions{
		Model:          model,
		ConversationID: "conv_abc",
	})
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
		{Output: []TResponseOutputItem{functionCallOutput(t, "echo", "call_1", "{}")}, Usage: NewUsage(), ResponseID: "resp_1"},
		{Output: []TResponseOutputItem{messageOutput(t, "done")}, Usage: NewUsage(), ResponseID: "resp_2"},
	}}
	echo := NewFunctionTool("echo", "echo", func(context.Context, *ToolContext, struct{}) (string, error) {
		return "ok", nil
	})
	agent := &Agent{Name: "a", Model: "m", Tools: []Tool{echo}}

	_, err := Run(context.Background(), agent, "hello", RunOptions{
		Model:          model,
		ConversationID: "conv_abc",
	})
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
	_, err := Run(context.Background(), agent, "hi", RunOptions{
		Model:          &recordingModel{},
		ConversationID: "conv_abc",
		Session:        NewInMemorySession(),
	})
	if err == nil {
		t.Fatal("expected error combining ConversationID with a Session")
	}
}
