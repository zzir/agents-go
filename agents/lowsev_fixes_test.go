package agents

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// A message whose only content is a refusal must surface as ModelRefusalError,
// not as a silent empty final output.
func TestRun_RefusalSurfacesAsError(t *testing.T) {
	raw := `{"type":"message","id":"msg_1","status":"completed","role":"assistant",` +
		`"content":[{"type":"refusal","refusal":"I cannot help with that."}]}`
	model := &fakeModel{responses: []*ModelResponse{modelResp(mustOutputItem(t, raw))}}
	agent := &Agent{Name: "a", ModelImpl: model}

	_, err := Run(context.Background(), agent, "do it", RunOptions{})
	if err == nil {
		t.Fatal("expected ModelRefusalError")
	}
	var refusal *ModelRefusalError
	if !errors.As(err, &refusal) {
		t.Fatalf("err = %T(%v), want *ModelRefusalError", err, err)
	}
	if refusal.Refusal != "I cannot help with that." {
		t.Errorf("refusal = %q", refusal.Refusal)
	}
	if refusal.Details == nil {
		t.Error("expected RunErrorDetails on the refusal error")
	}
}

// FunctionTool.Timeout must cancel the invocation and produce ToolTimeoutError.
func TestRun_ToolTimeout(t *testing.T) {
	tool := NewFunctionTool("slow", "", func(ctx context.Context, tc *ToolContext, args struct{}) (string, error) {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(5 * time.Second):
			return "done", nil
		}
	})
	tool.Timeout = 20 * time.Millisecond
	tool.FailureErrorFunction = nil // make the timeout fatal so we can assert the type
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "slow", "c1", `{}`)),
		modelResp(messageOutput(t, "done")),
	}}
	agent := &Agent{Name: "a", Tools: []Tool{tool}, ModelImpl: model}

	_, err := Run(context.Background(), agent, "go", RunOptions{})
	var te *ToolTimeoutError
	if !errors.As(err, &te) {
		t.Fatalf("err = %T(%v), want *ToolTimeoutError", err, err)
	}
	if te.ToolName != "slow" {
		t.Errorf("tool name = %q", te.ToolName)
	}

	// With the default FailureErrorFunction the timeout is fed back to the model.
	tool2 := NewFunctionTool("slow2", "", func(ctx context.Context, tc *ToolContext, args struct{}) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	})
	tool2.Timeout = 20 * time.Millisecond
	model2 := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "slow2", "c1", `{}`)),
		modelResp(messageOutput(t, "recovered")),
	}}
	agent2 := &Agent{Name: "a", Tools: []Tool{tool2}, ModelImpl: model2}
	res, err := Run(context.Background(), agent2, "go", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.FinalOutputString() != "recovered" {
		t.Errorf("final = %q", res.FinalOutputString())
	}
}

// Agent-level hooks must fire for tools and handoffs, mirroring Python's AgentHooks.
type recordingAgentHooks struct {
	BaseAgentHooks
	events *[]string
}

func (h recordingAgentHooks) OnToolStart(_ context.Context, _ *RunContext, agent *Agent, tool Tool) error {
	*h.events = append(*h.events, "tool_start:"+agent.Name+":"+tool.ToolName())
	return nil
}

func (h recordingAgentHooks) OnToolEnd(_ context.Context, _ *RunContext, agent *Agent, tool Tool, _ any) error {
	*h.events = append(*h.events, "tool_end:"+agent.Name+":"+tool.ToolName())
	return nil
}

func (h recordingAgentHooks) OnHandoff(_ context.Context, _ *RunContext, agent, source *Agent) error {
	*h.events = append(*h.events, "handoff:"+source.Name+"->"+agent.Name)
	return nil
}

func TestAgentHooks_ToolAndHandoffCallbacks(t *testing.T) {
	var events []string
	tool := NewFunctionTool("noop", "", func(ctx context.Context, tc *ToolContext, a struct{}) (string, error) {
		return "ok", nil
	})

	target := &Agent{
		Name:      "target",
		Hooks:     recordingAgentHooks{events: &events},
		ModelImpl: &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "done"))}},
	}
	src := &Agent{
		Name:  "src",
		Hooks: recordingAgentHooks{events: &events},
		Tools: []Tool{tool},
		ModelImpl: &fakeModel{responses: []*ModelResponse{
			modelResp(functionCallOutput(t, "noop", "c1", `{}`)),
			modelResp(functionCallOutput(t, "transfer_to_target", "c2", `{}`)),
		}},
		Handoffs: []Handoff{HandoffTo(target)},
	}

	if _, err := Run(context.Background(), src, "go", RunOptions{}); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(events, ",")
	for _, want := range []string{"tool_start:src:noop", "tool_end:src:noop", "handoff:src->target"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in events: %v", want, events)
		}
	}
}

// Wrapped SDK errors must still receive RunErrorDetails via errors.As unwrapping.
func TestRunErrorDetails_AttachedThroughWrapping(t *testing.T) {
	inner := &ModelBehaviorError{AgentsError{Message: "boom"}}
	wrapped := fmt.Errorf("outer context: %w", inner)
	var ae *AgentsError
	if !asAgentsError(wrapped, &ae) {
		t.Fatal("asAgentsError should unwrap wrapped SDK errors")
	}
	if ae != &inner.AgentsError {
		t.Error("expected the embedded AgentsError of the wrapped error")
	}
}

// Structured output: a model response missing a root-level required key must
// error instead of silently decoding to a zero value.
func TestOutputType_MissingRequiredKeyErrors(t *testing.T) {
	type out struct {
		Name string `json:"name"`
		Age  int    `json:"age"`
	}
	s := OutputType[out]()
	if _, err := s.ValidateJSON(`{"name":"ada"}`); err == nil || !strings.Contains(err.Error(), `"age"`) {
		t.Errorf("err = %v, want missing required key \"age\"", err)
	}
	v, err := s.ValidateJSON(`{"name":"ada","age":36}`)
	if err != nil {
		t.Fatal(err)
	}
	if v.(out).Age != 36 {
		t.Errorf("v = %#v", v)
	}
}

// A nested agent-as-tool run must join the parent's trace instead of starting
// (and finishing) a second root trace.
func TestAgentTool_JoinsParentTrace(t *testing.T) {
	col := &tracingCollector{}
	sub := &Agent{Name: "sub", ModelImpl: &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "sub says hi"))}}}
	parent := &Agent{
		Name:  "parent",
		Tools: []Tool{sub.AsTool(AgentToolConfig{Name: "ask_sub", Description: "ask"})},
		ModelImpl: &fakeModel{responses: []*ModelResponse{
			modelResp(functionCallOutput(t, "ask_sub", "c1", `{"input":"hi"}`)),
			modelResp(messageOutput(t, "done")),
		}},
	}

	if _, err := Run(context.Background(), parent, "go", RunOptions{Tracer: newTestTracer(col)}); err != nil {
		t.Fatal(err)
	}
	if col.traces != 1 {
		t.Errorf("traces = %d, want 1 (nested run must not start its own)", col.traces)
	}
	// Both the parent's and the nested run's agent spans are recorded.
	if col.named("agent:") != 2 {
		t.Errorf("agent spans = %d, want 2 (parent + nested)", col.named("agent:"))
	}
}
