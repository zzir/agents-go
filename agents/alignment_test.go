package agents

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// #1 — tool error is fed back to the model by default.
func TestToolError_FeedsBackToModel(t *testing.T) {
	tool := NewFunctionTool("boom", "fails", func(ctx context.Context, tc *ToolContext, a struct{}) (string, error) {
		return "", errors.New("kaboom")
	})
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "boom", "c1", `{}`)),
		modelResp(messageOutput(t, "recovered")),
	}}
	agent := &Agent{Name: "a", Tools: []Tool{tool}, ModelImpl: model}

	res, err := Run(context.Background(), agent, "go", RunOptions{})
	if err != nil {
		t.Fatalf("run should not fail when tool error is fed back: %v", err)
	}
	if res.FinalOutputString() != "recovered" {
		t.Errorf("final = %q", res.FinalOutputString())
	}
	// The error message should appear as a tool output item.
	var found bool
	for _, it := range res.NewItems {
		if o, ok := it.(*ToolCallOutputItem); ok {
			if s, _ := o.Output.(string); strings.Contains(s, "kaboom") {
				found = true
			}
		}
	}
	if !found {
		t.Error("expected tool error message fed back as tool output")
	}
}

// #1 — setting FailureErrorFunction to nil makes tool errors fatal.
func TestToolError_FatalWhenNil(t *testing.T) {
	tool := NewFunctionTool("boom", "fails", func(ctx context.Context, tc *ToolContext, a struct{}) (string, error) {
		return "", errors.New("kaboom")
	})
	tool.FailureErrorFunction = nil // opt into raising
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "boom", "c1", `{}`)),
	}}
	agent := &Agent{Name: "a", Tools: []Tool{tool}, ModelImpl: model}

	if _, err := Run(context.Background(), agent, "go", RunOptions{}); err == nil {
		t.Fatal("expected run to fail when FailureErrorFunction is nil")
	}
}

// #5 — tool_use_behavior callback can stop with a custom final output.
func TestToolUseBehaviorFunc(t *testing.T) {
	tool := NewFunctionTool("calc", "", func(ctx context.Context, tc *ToolContext, a struct{}) (int, error) {
		return 42, nil
	})
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "calc", "c1", `{}`)),
	}}
	agent := &Agent{
		Name:      "a",
		Tools:     []Tool{tool},
		ModelImpl: model,
		ToolUseBehavior: ToolUseBehaviorFunc(func(ctx context.Context, rc *RunContext, results []FunctionToolResult) (bool, any, error) {
			if len(results) == 1 && results[0].ToolName == "calc" {
				return true, "computed:42", nil
			}
			return false, nil, nil
		}),
	}

	res, err := Run(context.Background(), agent, "go", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.FinalOutputString() != "computed:42" {
		t.Errorf("final = %q", res.FinalOutputString())
	}
	if model.calls != 1 {
		t.Errorf("model calls = %d, want 1 (behavior func stopped the run)", model.calls)
	}
}

// #2 — handoff OnHandoff callback fires and InputFilter trims the next agent's input.
func TestHandoff_OnHandoffAndInputFilter(t *testing.T) {
	var callbackFired bool

	target := &Agent{Name: "target"}
	target.ModelImpl = &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "target done"))}}

	h := HandoffTo(target)
	h.OnHandoff = func(ctx context.Context, rc *RunContext, argsJSON string) error {
		callbackFired = true
		return nil
	}
	h.InputFilter = func(_ HandoffInputData) HandoffInputData {
		// Drop everything, give the target a single fresh message.
		return HandoffInputData{InputHistory: InputItemsFromText("fresh start")}
	}

	src := &Agent{
		Name:      "src",
		ModelImpl: &fakeModel{responses: []*ModelResponse{modelResp(functionCallOutput(t, "transfer_to_target", "c1", `{}`))}},
		Handoffs:  []Handoff{h},
	}

	targetModel := target.ModelImpl.(*fakeModel)
	res, err := Run(context.Background(), src, "original long conversation", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !callbackFired {
		t.Error("OnHandoff callback did not fire")
	}
	if res.LastAgent != target {
		t.Errorf("last agent = %v, want target", res.LastAgent.Name)
	}
	// The target agent should have seen the filtered (single-item) input.
	if len(targetModel.lastReq.Input) != 1 {
		t.Errorf("target input not filtered: %d items, want 1", len(targetModel.lastReq.Input))
	}
}

// #4 — previous_response_id mode sends only new items and chains the response ID.
func modelRespID(id string, items ...TResponseOutputItem) *ModelResponse {
	r := modelResp(items...)
	r.ResponseID = id
	return r
}

func TestPreviousResponseID(t *testing.T) {
	tool := NewFunctionTool("noop", "", func(ctx context.Context, tc *ToolContext, a struct{}) (string, error) {
		return "ok", nil
	})
	model := &fakeModel{responses: []*ModelResponse{
		modelRespID("resp_1", functionCallOutput(t, "noop", "c1", `{}`)),
		modelRespID("resp_2", messageOutput(t, "done")),
	}}
	agent := &Agent{Name: "a", Tools: []Tool{tool}, ModelImpl: model}

	res, err := Run(context.Background(), agent, "go", RunOptions{UsePreviousResponseID: true})
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
	tool := NewFunctionTool("noop", "", func(ctx context.Context, tc *ToolContext, a struct{}) (string, error) {
		return "ok", nil
	})
	model := &fakeModel{responses: []*ModelResponse{
		modelRespID("resp_1", functionCallOutput(t, "noop", "c1", `{}`)),
		modelRespID("resp_2", messageOutput(t, "done")),
	}}
	agent := &Agent{Name: "a", Tools: []Tool{tool}, ModelImpl: model}

	if _, err := Run(context.Background(), agent, "go", RunOptions{}); err != nil {
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

// #1 — streaming loop also honors previous_response_id.
func TestPreviousResponseID_Streaming(t *testing.T) {
	tool := NewFunctionTool("noop", "", func(ctx context.Context, tc *ToolContext, a struct{}) (string, error) {
		return "ok", nil
	})
	model := &fakeModel{responses: []*ModelResponse{
		modelRespID("resp_1", functionCallOutput(t, "noop", "c1", `{}`)),
		modelRespID("resp_2", messageOutput(t, "done")),
	}}
	agent := &Agent{Name: "a", Tools: []Tool{tool}, ModelImpl: model}

	sr := RunStreamed(context.Background(), agent, "go", RunOptions{UsePreviousResponseID: true})
	for _, err := range sr.Events() {
		if err != nil {
			t.Fatal(err)
		}
	}
	res, err := sr.FinalResult()
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
