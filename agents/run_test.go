package agents

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"testing"

	"github.com/openai/openai-go/v3/responses"
)

// fakeModel is a scripted Model for testing the runner without a real API. Each
// call to GetResponse returns the next queued response. It mirrors the Python
// test suite's FakeModel.
type fakeModel struct {
	responses []*ModelResponse
	idx       int
	lastReq   ModelRequest
	calls     int
}

func (m *fakeModel) GetResponse(ctx context.Context, req ModelRequest) (*ModelResponse, error) {
	m.lastReq = req
	m.calls++
	if m.idx >= len(m.responses) {
		return &ModelResponse{Output: nil, Usage: NewUsage()}, nil
	}
	resp := m.responses[m.idx]
	m.idx++
	if resp.Usage == nil {
		resp.Usage = &Usage{Requests: 1, TotalTokens: 1, InputTokens: 1}
	}
	return resp, nil
}

func (m *fakeModel) StreamResponse(ctx context.Context, req ModelRequest) iter.Seq2[*TResponseStreamEvent, error] {
	return func(yield func(*TResponseStreamEvent, error) bool) {
		m.lastReq = req
		m.calls++
		var resp *ModelResponse
		if m.idx < len(m.responses) {
			resp = m.responses[m.idx]
			m.idx++
		} else {
			resp = &ModelResponse{Usage: NewUsage()}
		}
		// Emit a single response.completed event carrying the queued response.
		event := completedStreamEvent(resp)
		yield(&event, nil)
	}
}

// completedStreamEvent builds a response.completed stream event whose embedded
// Response carries the given output items, so the streaming runner can assemble
// a ModelResponse from it.
func completedStreamEvent(resp *ModelResponse) TResponseStreamEvent {
	rawItems := make([]json.RawMessage, 0, len(resp.Output))
	for i := range resp.Output {
		rawItems = append(rawItems, json.RawMessage(resp.Output[i].RawJSON()))
	}
	outBytes, _ := json.Marshal(rawItems)
	payload := `{"type":"response.completed","sequence_number":0,"response":{"id":"resp_1","output":` +
		string(outBytes) + `,"usage":{"input_tokens":5,"output_tokens":3,"total_tokens":8,` +
		`"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":0}}}}`
	var event TResponseStreamEvent
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		panic(err)
	}
	return event
}

// --- output item builders (constructed via JSON so RawJSON is populated) ---

func mustOutputItem(t *testing.T, raw string) TResponseOutputItem {
	t.Helper()
	var item responses.ResponseOutputItemUnion
	if err := json.Unmarshal([]byte(raw), &item); err != nil {
		t.Fatalf("build output item: %v", err)
	}
	return item
}

func messageOutput(t *testing.T, text string) TResponseOutputItem {
	raw := `{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":` +
		quote(text) + `,"annotations":[]}]}`
	return mustOutputItem(t, raw)
}

func functionCallOutput(t *testing.T, name, callID, args string) TResponseOutputItem {
	raw := `{"type":"function_call","id":"fc_1","call_id":` + quote(callID) +
		`,"name":` + quote(name) + `,"arguments":` + quote(args) + `,"status":"completed"}`
	return mustOutputItem(t, raw)
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func modelResp(items ...TResponseOutputItem) *ModelResponse {
	return &ModelResponse{Output: items, Usage: &Usage{Requests: 1, InputTokens: 5, OutputTokens: 3, TotalTokens: 8}}
}

// --- tests ---

func TestRun_SingleTurnPlainText(t *testing.T) {
	model := &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "hello world"))}}
	agent := &Agent{Name: "assistant", Instructions: StaticInstructions("be nice"), ModelImpl: model}

	res, err := Run(context.Background(), agent, "hi", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.FinalOutputString() != "hello world" {
		t.Errorf("final output = %q", res.FinalOutputString())
	}
	if res.LastAgent != agent {
		t.Errorf("last agent mismatch")
	}
	if model.lastReq.SystemInstructions != "be nice" {
		t.Errorf("system instructions = %q", model.lastReq.SystemInstructions)
	}
	if res.Usage.TotalTokens != 8 {
		t.Errorf("usage total = %d, want 8", res.Usage.TotalTokens)
	}
}

func TestRun_ToolCallThenFinal(t *testing.T) {
	var toolCalled bool
	tool := NewFunctionTool("get_weather", "weather",
		func(ctx context.Context, tc *ToolContext, args struct {
			City string `json:"city"`
		}) (string, error) {
			toolCalled = true
			if args.City != "SF" {
				t.Errorf("city = %q", args.City)
			}
			return "sunny", nil
		})

	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "get_weather", "call_1", `{"city":"SF"}`)),
		modelResp(messageOutput(t, "it is sunny")),
	}}
	agent := &Agent{Name: "a", Tools: []Tool{tool}, ModelImpl: model}

	res, err := Run(context.Background(), agent, "weather in SF?", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !toolCalled {
		t.Error("tool was not called")
	}
	if res.FinalOutputString() != "it is sunny" {
		t.Errorf("final = %q", res.FinalOutputString())
	}
	if model.calls != 2 {
		t.Errorf("model calls = %d, want 2", model.calls)
	}
	// The second turn's input must include the tool call and its output.
	if len(model.lastReq.Input) < 3 {
		t.Errorf("second turn input too short: %d items", len(model.lastReq.Input))
	}
}

type sentiment struct {
	Label string `json:"label"`
	Score int    `json:"score"`
}

func TestRun_StructuredOutput(t *testing.T) {
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(messageOutput(t, `{"label":"positive","score":9}`)),
	}}
	agent := &Agent{Name: "a", OutputType: OutputType[sentiment](), ModelImpl: model}

	res, err := Run(context.Background(), agent, "analyze", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got, ok := FinalOutputAs[sentiment](res)
	if !ok {
		t.Fatalf("final output type = %T", res.FinalOutput)
	}
	if got.Label != "positive" || got.Score != 9 {
		t.Errorf("got %+v", got)
	}
}

func TestRun_MaxTurnsExceeded(t *testing.T) {
	// Always returns a tool call, never a final output -> loops until max turns.
	tool := NewFunctionTool("loop", "loops",
		func(ctx context.Context, tc *ToolContext, args struct{}) (string, error) {
			return "again", nil
		})
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "loop", "c1", `{}`)),
		modelResp(functionCallOutput(t, "loop", "c2", `{}`)),
		modelResp(functionCallOutput(t, "loop", "c3", `{}`)),
	}}
	agent := &Agent{Name: "a", Tools: []Tool{tool}, ModelImpl: model}

	_, err := Run(context.Background(), agent, "go", RunOptions{MaxTurns: 2})
	if err == nil {
		t.Fatal("expected MaxTurnsError")
	}
	if !errors.Is(err, ErrMaxTurns) {
		t.Errorf("error = %v, want ErrMaxTurns", err)
	}
	var mte *MaxTurnsError
	if !errors.As(err, &mte) {
		t.Errorf("error not a *MaxTurnsError: %T", err)
	}
}

func TestRun_StopOnFirstTool(t *testing.T) {
	tool := NewFunctionTool("compute", "computes",
		func(ctx context.Context, tc *ToolContext, args struct{}) (string, error) {
			return "the-answer", nil
		})
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "compute", "c1", `{}`)),
	}}
	agent := &Agent{Name: "a", Tools: []Tool{tool}, ToolUseBehavior: StopOnFirstTool{}, ModelImpl: model}

	res, err := Run(context.Background(), agent, "go", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.FinalOutputString() != "the-answer" {
		t.Errorf("final = %q, want the-answer", res.FinalOutputString())
	}
	if model.calls != 1 {
		t.Errorf("model calls = %d, want 1 (stop on first tool)", model.calls)
	}
}

func TestRun_UnknownToolErrors(t *testing.T) {
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "ghost", "c1", `{}`)),
	}}
	agent := &Agent{Name: "a", ModelImpl: model}

	_, err := Run(context.Background(), agent, "go", RunOptions{})
	if err == nil {
		t.Fatal("expected error for unknown tool")
	}
	var mbe *ModelBehaviorError
	if !errors.As(err, &mbe) {
		t.Errorf("error not *ModelBehaviorError: %T (%v)", err, err)
	}
}

func TestRun_Handoff(t *testing.T) {
	billing := &Agent{Name: "billing"}
	billingModel := &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "billing handled it"))}}
	billing.ModelImpl = billingModel

	triageModel := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "transfer_to_billing", "c1", `{}`)),
	}}
	triage := &Agent{
		Name:      "triage",
		ModelImpl: triageModel,
		Handoffs:  []Handoff{HandoffTo(billing)},
	}

	res, err := Run(context.Background(), triage, "I have a billing question", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.LastAgent != billing {
		t.Errorf("last agent = %v, want billing", res.LastAgent.Name)
	}
	if res.FinalOutputString() != "billing handled it" {
		t.Errorf("final = %q", res.FinalOutputString())
	}
}

func TestRun_ParallelTools(t *testing.T) {
	var aCalled, bCalled bool
	toolA := NewFunctionTool("tool_a", "", func(ctx context.Context, tc *ToolContext, args struct{}) (string, error) {
		aCalled = true
		return "a-done", nil
	})
	toolB := NewFunctionTool("tool_b", "", func(ctx context.Context, tc *ToolContext, args struct{}) (string, error) {
		bCalled = true
		return "b-done", nil
	})
	model := &fakeModel{responses: []*ModelResponse{
		{Output: []TResponseOutputItem{
			functionCallOutput(t, "tool_a", "c1", `{}`),
			functionCallOutput(t, "tool_b", "c2", `{}`),
		}, Usage: NewUsage()},
		modelResp(messageOutput(t, "both done")),
	}}
	agent := &Agent{Name: "a", Tools: []Tool{toolA, toolB}, ModelImpl: model}

	res, err := Run(context.Background(), agent, "do both", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !aCalled || !bCalled {
		t.Errorf("tools called: a=%v b=%v", aCalled, bCalled)
	}
	if res.FinalOutputString() != "both done" {
		t.Errorf("final = %q", res.FinalOutputString())
	}
}

func TestRun_IsEnabledHidesTool(t *testing.T) {
	tool := NewFunctionTool("secret", "", func(ctx context.Context, tc *ToolContext, args struct{}) (string, error) {
		return "", nil
	})
	tool.IsEnabled = func(ctx context.Context, rc *RunContext, agent *Agent) (bool, error) {
		return false, nil
	}
	model := &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "ok"))}}
	agent := &Agent{Name: "a", Tools: []Tool{tool}, ModelImpl: model}

	if _, err := Run(context.Background(), agent, "hi", RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(model.lastReq.Tools) != 0 {
		t.Errorf("disabled tool should be hidden, got %d tools", len(model.lastReq.Tools))
	}
}
