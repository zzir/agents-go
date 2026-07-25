package agents

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
)

// TestToolContext_EnrichedFields verifies that a function tool sees the enriched
// ToolContext: the active Agent, the raw ToolCall, and the turn input.
func TestToolContext_EnrichedFields(t *testing.T) {
	var gotAgent *Agent
	var gotCallID, gotCallName string
	var turnInputLen int
	agent := &Agent{Name: "a"}
	tool := NewFunctionTool("probe", "probe",
		func(ctx context.Context, tc *ToolContext, args struct{}) (string, error) {
			gotAgent = tc.Agent
			gotCallID = tc.ToolCall.AsFunctionCall().CallID
			gotCallName = tc.ToolCall.AsFunctionCall().Name
			turnInputLen = len(tc.TurnInput())
			return "ok", nil
		})
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "probe", "call_1", `{}`)),
		modelResp(messageOutput(t, "done")),
	}}
	agent.Tools = []Tool{tool}
	agent.ModelImpl = model

	if _, err := RunSync(context.Background(), agent, "hello", RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if gotAgent != agent {
		t.Errorf("tc.Agent = %v, want the running agent", gotAgent)
	}
	if gotCallID != "call_1" {
		t.Errorf("tc.ToolCall call id = %q, want call_1", gotCallID)
	}
	if gotCallName != "probe" {
		t.Errorf("tc.ToolCall name = %q, want probe", gotCallName)
	}
	if turnInputLen == 0 {
		t.Errorf("tc.TurnInput is empty; want the turn's input items")
	}
}

// captureToolHooks records the ToolContext passed to the tool lifecycle hooks so
// concurrent calls can be distinguished by call id.
type captureToolHooks struct {
	BaseRunHooks
	startIDs []string
	endIDs   []string
}

func (h *captureToolHooks) OnToolStart(_ context.Context, tc *ToolContext, _ *Agent, _ Tool) error {
	h.startIDs = append(h.startIDs, tc.ToolCallID)
	return nil
}

func (h *captureToolHooks) OnToolEnd(_ context.Context, tc *ToolContext, _ *Agent, _ Tool, _ any) error {
	h.endIDs = append(h.endIDs, tc.ToolCallID)
	return nil
}

// TestToolHooks_ReceiveToolContext verifies OnToolStart/OnToolEnd receive the
// per-call ToolContext instead of only the shared RunContext.
func TestToolHooks_ReceiveToolContext(t *testing.T) {
	tool := NewFunctionTool("probe", "probe",
		func(ctx context.Context, tc *ToolContext, args struct{}) (string, error) {
			return "ok", nil
		})
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "probe", "call_42", `{}`)),
		modelResp(messageOutput(t, "done")),
	}}
	agent := &Agent{Name: "a", Tools: []Tool{tool}, ModelImpl: model}

	hooks := &captureToolHooks{}
	if _, err := RunSync(context.Background(), agent, "hi", RunOptions{Hooks: hooks}); err != nil {
		t.Fatal(err)
	}
	if len(hooks.startIDs) != 1 || hooks.startIDs[0] != "call_42" {
		t.Errorf("OnToolStart call ids = %v, want [call_42]", hooks.startIDs)
	}
	if len(hooks.endIDs) != 1 || hooks.endIDs[0] != "call_42" {
		t.Errorf("OnToolEnd call ids = %v, want [call_42]", hooks.endIDs)
	}
}

// TestNeedsApprovalFunc_ReceivesCallID verifies the per-call approval predicate
// receives the tool call id (Python parity: needs_approval(ctx, params, call_id)).
func TestNeedsApprovalFunc_ReceivesCallID(t *testing.T) {
	var gotCallID atomic.Value
	tool := NewFunctionTool("deploy", "deploys",
		func(ctx context.Context, tc *ToolContext, args struct{}) (string, error) {
			return "ok", nil
		})
	tool.NeedsApprovalFunc = func(ctx context.Context, rc *RunContext, argsJSON, callID string) (bool, error) {
		gotCallID.Store(callID)
		return false, nil
	}
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "deploy", "call_99", `{}`)),
		modelResp(messageOutput(t, "done")),
	}}
	agent := &Agent{Name: "a", Tools: []Tool{tool}, ModelImpl: model}

	if _, err := RunSync(context.Background(), agent, "go", RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if got, _ := gotCallID.Load().(string); got != "call_99" {
		t.Errorf("NeedsApprovalFunc call id = %q, want call_99", got)
	}
}

// handoffWithRequiredInput builds a handoff whose input schema requires a key.
func handoffWithRequiredInput(target *Agent) Handoff {
	return Handoff{
		ToolName:        "transfer_to_" + target.Name,
		ToolDescription: "transfer",
		InputJSONSchema: map[string]any{
			"type":                 "object",
			"properties":           map[string]any{"reason": map[string]any{"type": "string"}},
			"required":             []any{"reason"},
			"additionalProperties": false,
		},
		StrictJSONSchema: true,
		AgentName:        target.Name,
		OnInvoke: func(context.Context, *RunContext, string) (*Agent, error) {
			return target, nil
		},
	}
}

// TestHandoff_MissingRequiredInput verifies a handoff that expects input fails
// with *ModelBehaviorError when the model sends none.
func TestHandoff_MissingRequiredInput(t *testing.T) {
	target := &Agent{Name: "billing"}
	source := &Agent{Name: "triage"}
	h := handoffWithRequiredInput(target)
	source.Handoffs = []Handoff{h}

	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, h.ToolName, "call_1", ``)),
	}}
	source.ModelImpl = model

	_, err := RunSync(context.Background(), source, "please", RunOptions{})
	var mbe *ModelBehaviorError
	if !errors.As(err, &mbe) {
		t.Fatalf("err = %v (%T), want *ModelBehaviorError", err, err)
	}
	if !strings.Contains(mbe.Error(), "expected non-null input") {
		t.Errorf("message = %q, want the non-null-input parity text", mbe.Error())
	}
}

// TestHandoff_InvalidRequiredKey verifies an object missing a required key is
// rejected too.
func TestHandoff_InvalidRequiredKey(t *testing.T) {
	target := &Agent{Name: "billing"}
	source := &Agent{Name: "triage"}
	h := handoffWithRequiredInput(target)
	source.Handoffs = []Handoff{h}

	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, h.ToolName, "call_1", `{"other":"x"}`)),
	}}
	source.ModelImpl = model

	_, err := RunSync(context.Background(), source, "please", RunOptions{})
	var mbe *ModelBehaviorError
	if !errors.As(err, &mbe) {
		t.Fatalf("err = %v (%T), want *ModelBehaviorError", err, err)
	}
}

// TestHandoff_ValidInputSucceeds verifies a well-formed input transfers control.
func TestHandoff_ValidInputSucceeds(t *testing.T) {
	target := &Agent{Name: "billing"}
	source := &Agent{Name: "triage"}
	h := handoffWithRequiredInput(target)
	source.Handoffs = []Handoff{h}

	srcModel := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, h.ToolName, "call_1", `{"reason":"needs billing"}`)),
	}}
	source.ModelImpl = srcModel
	target.ModelImpl = &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "handled"))}}

	res, err := RunSync(context.Background(), source, "please", RunOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.LastAgent != target {
		t.Errorf("last agent = %v, want billing target", res.LastAgent)
	}
}

// TestToolNotFound_ReturnToModelText verifies the model-visible text for an
// unknown tool matches the upstream default "Tool 'X' not found.".
func TestToolNotFound_ReturnToModelText(t *testing.T) {
	agent := &Agent{Name: "a"}
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "ghost", "call_1", `{}`)),
		modelResp(messageOutput(t, "recovered")),
	}}
	agent.ModelImpl = model

	res, err := RunSync(context.Background(), agent, "hi", RunOptions{ToolNotFoundBehavior: ToolNotFoundReturnToModel})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, it := range res.NewItems {
		out, ok := it.(*ToolCallOutputItem)
		if !ok {
			continue
		}
		text, _ := out.Output.(string)
		if text == "Tool 'ghost' not found." {
			found = true
		}
		if strings.HasPrefix(text, "Error:") {
			t.Errorf("tool-not-found output kept the Error: prefix: %q", text)
		}
	}
	if !found {
		t.Errorf("did not find the expected \"Tool 'ghost' not found.\" output")
	}
}

// TestParseToolNotFoundBehavior_Alias verifies the upstream string alias.
func TestParseToolNotFoundBehavior_Alias(t *testing.T) {
	for _, s := range []string{"return_to_model", "return_error_to_model"} {
		if got := ParseToolNotFoundBehavior(s); got != ToolNotFoundReturnToModel {
			t.Errorf("ParseToolNotFoundBehavior(%q) = %v, want ToolNotFoundReturnToModel", s, got)
		}
	}
	if got := ParseToolNotFoundBehavior("nonsense"); got != ToolNotFoundError {
		t.Errorf("ParseToolNotFoundBehavior(nonsense) = %v, want ToolNotFoundError", got)
	}
}
