package agents

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"testing"
)

// eventStreamModel yields a fixed event sequence, then an optional error. Its
// Respond counts calls so tests can prove the adapter never uses it.
type eventStreamModel struct {
	events      []ResponseStreamEvent
	err         error
	getCalls    int
	streamCalls int
}

func (m *eventStreamModel) Respond(context.Context, ModelRequest) (*ModelResponse, error) {
	m.getCalls++
	return &ModelResponse{ResponseID: "blocking"}, nil
}

func (m *eventStreamModel) StreamResponse(context.Context, ModelRequest) iter.Seq2[*ResponseStreamEvent, error] {
	return func(yield func(*ResponseStreamEvent, error) bool) {
		m.streamCalls++
		for i := range m.events {
			if !yield(&m.events[i], nil) {
				return
			}
		}
		if m.err != nil {
			yield(nil, m.err)
		}
	}
}

// mustStreamEvent builds a stream event via JSON so the union's As* accessors
// work (a hand-filled struct would leave the raw payload empty).
func mustStreamEvent(t *testing.T, raw string) ResponseStreamEvent {
	t.Helper()
	var ev ResponseStreamEvent
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		t.Fatalf("build stream event: %v", err)
	}
	return ev
}

const streamOnlyMessageItem = `{"type":"message","id":"m1","role":"assistant","status":"completed",` +
	`"content":[{"type":"output_text","text":"hi","annotations":[]}]}`

func TestStreamOnlyModel_GetResponseAssemblesFromStream(t *testing.T) {
	inner := &eventStreamModel{events: []ResponseStreamEvent{
		mustStreamEvent(t, `{"type":"response.created","response":{"id":"resp_1"}}`),
		mustStreamEvent(t, `{"type":"response.completed","response":{"id":"resp_1","status":"completed",`+
			`"output":[`+streamOnlyMessageItem+`],"usage":{"input_tokens":5,"output_tokens":3,"total_tokens":8,`+
			`"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":0}}}}`),
	}}
	m := NewStreamOnlyModel(inner)

	resp, err := m.Respond(context.Background(), ModelRequest{})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if resp.ResponseID != "resp_1" || resp.Status != "completed" {
		t.Errorf("ResponseID=%q Status=%q, want resp_1/completed", resp.ResponseID, resp.Status)
	}
	if len(resp.Output) != 1 {
		t.Fatalf("len(Output) = %d, want 1", len(resp.Output))
	}
	if resp.Usage == nil || resp.Usage.InputTokens != 5 || resp.Usage.OutputTokens != 3 || resp.Usage.Requests != 1 {
		t.Errorf("Usage = %+v, want input 5 / output 3 / requests 1", resp.Usage)
	}
	if inner.getCalls != 0 || inner.streamCalls != 1 {
		t.Errorf("getCalls=%d streamCalls=%d, want 0/1", inner.getCalls, inner.streamCalls)
	}
}

func TestStreamOnlyModel_IncompleteResponseAssembles(t *testing.T) {
	// A length-truncated response still arrived; like the blocking path, it is
	// assembled (with its reason) rather than failed, so the runner can refuse
	// its tool calls and re-ask instead of losing the turn.
	inner := &eventStreamModel{events: []ResponseStreamEvent{
		mustStreamEvent(t, `{"type":"response.incomplete","response":{"id":"resp_2","status":"incomplete",`+
			`"output":[`+streamOnlyMessageItem+`],"incomplete_details":{"reason":"max_output_tokens"}}}`),
	}}
	resp, err := NewStreamOnlyModel(inner).Respond(context.Background(), ModelRequest{})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if resp.Status != "incomplete" || resp.IncompleteReason != "max_output_tokens" {
		t.Errorf("Status=%q IncompleteReason=%q", resp.Status, resp.IncompleteReason)
	}
}

func TestStreamOnlyModel_EmptyOutputFallsBackToStreamedItems(t *testing.T) {
	// ChatGPT with store=false sends a completed event whose Output is empty;
	// the adapter must fall back to items collected from output_item.done.
	inner := &eventStreamModel{events: []ResponseStreamEvent{
		mustStreamEvent(t, `{"type":"response.output_item.done","output_index":0,"item":`+streamOnlyMessageItem+`}`),
		mustStreamEvent(t, `{"type":"response.completed","response":{"id":"resp_3","status":"completed","output":[]}}`),
	}}
	resp, err := NewStreamOnlyModel(inner).Respond(context.Background(), ModelRequest{})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if len(resp.Output) != 1 {
		t.Fatalf("len(Output) = %d, want 1 (assembled from output_item.done)", len(resp.Output))
	}
}

func TestStreamOnlyModel_StreamErrorPropagates(t *testing.T) {
	inner := &eventStreamModel{
		events: []ResponseStreamEvent{mustStreamEvent(t, `{"type":"response.created","response":{"id":"r"}}`)},
		err:    errBoom,
	}
	_, err := NewStreamOnlyModel(inner).Respond(context.Background(), ModelRequest{})
	if !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want errBoom", err)
	}
}

func TestStreamOnlyModel_NoTerminalEventErrors(t *testing.T) {
	inner := &eventStreamModel{events: []ResponseStreamEvent{
		mustStreamEvent(t, `{"type":"response.created","response":{"id":"r"}}`),
	}}
	_, err := NewStreamOnlyModel(inner).Respond(context.Background(), ModelRequest{})
	var mbe *ModelBehaviorError
	if !errors.As(err, &mbe) {
		t.Fatalf("err = %v, want *ModelBehaviorError", err)
	}
}

func TestStreamOnlyModel_StreamResponsePassesThrough(t *testing.T) {
	inner := &eventStreamModel{events: []ResponseStreamEvent{
		mustStreamEvent(t, `{"type":"response.created","response":{"id":"r"}}`),
		mustStreamEvent(t, `{"type":"response.output_text.delta","delta":"hi"}`),
	}}
	events, err := drain(NewStreamOnlyModel(inner).StreamResponse(context.Background(), ModelRequest{}))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if events != 2 || inner.streamCalls != 1 {
		t.Errorf("events=%d streamCalls=%d, want 2/1", events, inner.streamCalls)
	}
}

func TestStreamOnlyProvider_WrapsModels(t *testing.T) {
	inner := &eventStreamModel{events: []ResponseStreamEvent{
		mustStreamEvent(t, `{"type":"response.completed","response":{"id":"resp_p","status":"completed","output":[]}}`),
	}}
	p := NewStreamOnlyProvider(&stubProvider{model: inner})
	m, err := p.Model("gpt-test")
	if err != nil {
		t.Fatalf("Model: %v", err)
	}
	resp, err := m.Respond(context.Background(), ModelRequest{})
	if err != nil {
		t.Fatalf("Respond: %v", err)
	}
	if resp.ResponseID != "resp_p" || inner.getCalls != 0 {
		t.Errorf("ResponseID=%q getCalls=%d, want resp_p/0", resp.ResponseID, inner.getCalls)
	}
}

func TestStreamOnlyProvider_PropagatesGetModelError(t *testing.T) {
	p := NewStreamOnlyProvider(errModelProvider{err: errBoom})
	if _, err := p.Model("x"); !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want errBoom", err)
	}
}
