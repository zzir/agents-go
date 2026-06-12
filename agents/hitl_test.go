package agents

import (
	"context"
	"sync"
	"testing"

	"github.com/zzir/agents-go/tracing"
)

func approvalAgentAndModel(t *testing.T, ran *bool) (*Agent, *fakeModel) {
	t.Helper()
	tool := NewFunctionTool("delete_db", "dangerous",
		func(ctx context.Context, tc *ToolContext, args struct{}) (string, error) {
			*ran = true
			return "deleted", nil
		})
	tool.NeedsApproval = true
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "delete_db", "call_1", `{}`)),
		modelResp(messageOutput(t, "all done")),
	}}
	agent := &Agent{Name: "a", Tools: []Tool{tool}, ModelImpl: model}
	return agent, model
}

func TestHITL_InterruptThenApprove(t *testing.T) {
	var ran bool
	agent, _ := approvalAgentAndModel(t, &ran)

	res, err := Run(context.Background(), agent, "delete it", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Interruptions) != 1 {
		t.Fatalf("expected 1 interruption, got %d", len(res.Interruptions))
	}
	if ran {
		t.Error("tool should not have run before approval")
	}
	if res.State == nil {
		t.Fatal("expected RunState on interruption")
	}

	// Approve and resume.
	res.State.Approve(res.Interruptions[0], false)
	res2, err := ResumeRun(context.Background(), res.State, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !ran {
		t.Error("tool should have run after approval")
	}
	if res2.FinalOutputString() != "all done" {
		t.Errorf("final = %q", res2.FinalOutputString())
	}
}

func TestHITL_InterruptThenReject(t *testing.T) {
	var ran bool
	tool := NewFunctionTool("delete_db", "dangerous",
		func(ctx context.Context, tc *ToolContext, args struct{}) (string, error) {
			ran = true
			return "deleted", nil
		})
	tool.NeedsApproval = true
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "delete_db", "call_1", `{}`)),
		modelResp(messageOutput(t, "okay, skipped")),
	}}
	agent := &Agent{Name: "a", Tools: []Tool{tool}, ModelImpl: model}

	res, err := Run(context.Background(), agent, "delete it", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	res.State.Reject(res.Interruptions[0], false, "denied by policy")
	res2, err := ResumeRun(context.Background(), res.State, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if ran {
		t.Error("tool should NOT run after rejection")
	}
	// The rejection message should appear as a tool output.
	var found bool
	for _, it := range res2.NewItems {
		if o, ok := it.(*ToolCallOutputItem); ok && o.Output == "denied by policy" {
			found = true
		}
	}
	if !found {
		t.Error("expected rejection message in tool outputs")
	}
	if res2.FinalOutputString() != "okay, skipped" {
		t.Errorf("final = %q", res2.FinalOutputString())
	}
}

func TestHITL_StateSerializationRoundTrip(t *testing.T) {
	var ran bool
	agent, _ := approvalAgentAndModel(t, &ran)

	res, err := Run(context.Background(), agent, "delete it", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// Serialize the paused state.
	data, err := res.State.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}

	// Rebuild from JSON with an agent registry (simulating a new process).
	registry := map[string]*Agent{"a": agent}
	state, err := RunStateFromJSON(data, registry)
	if err != nil {
		t.Fatal(err)
	}
	if state.CurrentAgent != agent {
		t.Error("current agent not restored")
	}
	if len(state.Interruptions) != 1 {
		t.Fatalf("interruptions not restored: %d", len(state.Interruptions))
	}

	// Approve on the rebuilt state and resume.
	state.Approve(state.Interruptions[0], false)
	res2, err := ResumeRun(context.Background(), state, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !ran {
		t.Error("tool should run after approval on restored state")
	}
	if res2.FinalOutputString() != "all done" {
		t.Errorf("final = %q", res2.FinalOutputString())
	}
}

func TestHITL_UnknownAgentInRegistry(t *testing.T) {
	var ran bool
	agent, _ := approvalAgentAndModel(t, &ran)
	res, _ := Run(context.Background(), agent, "go", RunOptions{})
	data, _ := res.State.MarshalJSON()

	_, err := RunStateFromJSON(data, map[string]*Agent{}) // empty registry
	if err == nil {
		t.Fatal("expected error for missing agent in registry")
	}
}

func TestRun_TracingEmitsSpans(t *testing.T) {
	col := &tracingCollector{}
	tr := newTestTracer(col)
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "noop", "c1", `{}`)),
		modelResp(messageOutput(t, "done")),
	}}
	tool := NewFunctionTool("noop", "", func(ctx context.Context, tc *ToolContext, a struct{}) (string, error) {
		return "ok", nil
	})
	agent := &Agent{Name: "tracer-agent", Tools: []Tool{tool}, ModelImpl: model}

	_, err := Run(context.Background(), agent, "go", RunOptions{Tracer: tr})
	if err != nil {
		t.Fatal(err)
	}
	if col.traces != 1 {
		t.Errorf("traces = %d, want 1", col.traces)
	}
	// One agent; two model calls -> two generation spans; one tool call.
	if col.named("agent:") != 1 {
		t.Errorf("agent spans = %d, want 1", col.named("agent:"))
	}
	if col.named("generation:") != 2 {
		t.Errorf("generation spans = %d, want 2", col.named("generation:"))
	}
	if col.named("function:") != 1 {
		t.Errorf("function spans = %d, want 1", col.named("function:"))
	}
}

func TestRun_TracingGuardrailAndHandoffSpans(t *testing.T) {
	col := &tracingCollector{}
	target := &Agent{Name: "target"}
	target.ModelImpl = &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "done"))}}

	src := &Agent{
		Name:      "src",
		ModelImpl: &fakeModel{responses: []*ModelResponse{modelResp(functionCallOutput(t, "transfer_to_target", "c1", `{}`))}},
		Handoffs:  []Handoff{HandoffTo(target)},
		InputGuardrails: []InputGuardrail{{
			Name: "g",
			Run: func(ctx context.Context, rc *RunContext, a *Agent, in []TResponseInputItem) (GuardrailFunctionOutput, error) {
				return GuardrailFunctionOutput{}, nil
			},
		}},
	}

	if _, err := Run(context.Background(), src, "go", RunOptions{Tracer: newTestTracer(col)}); err != nil {
		t.Fatal(err)
	}
	if col.named("agent:") != 2 {
		t.Errorf("agent spans = %d, want 2 (src + target)", col.named("agent:"))
	}
	if col.named("handoff:") != 1 {
		t.Errorf("handoff spans = %d, want 1", col.named("handoff:"))
	}
	if col.named("guardrail:input") != 1 {
		t.Errorf("input guardrail spans = %d, want 1", col.named("guardrail:input"))
	}
}

// tracingCollector is a thread-safe tracing.Processor that records span names.
// Spans can end from concurrent goroutines (e.g. input guardrails), so it locks.
type tracingCollector struct {
	mu        sync.Mutex
	traces    int
	spans     int
	spanNames []string
}

func (c *tracingCollector) OnTraceStart(*tracing.Trace) {}
func (c *tracingCollector) OnTraceEnd(*tracing.Trace) {
	c.mu.Lock()
	c.traces++
	c.mu.Unlock()
}
func (c *tracingCollector) OnSpanStart(*tracing.Span) {}
func (c *tracingCollector) OnSpanEnd(s *tracing.Span) {
	c.mu.Lock()
	c.spans++
	c.spanNames = append(c.spanNames, s.Name)
	c.mu.Unlock()
}
func (c *tracingCollector) ForceFlush()              {}
func (c *tracingCollector) Shutdown(context.Context) {}

func (c *tracingCollector) named(prefix string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, name := range c.spanNames {
		if len(name) >= len(prefix) && name[:len(prefix)] == prefix {
			n++
		}
	}
	return n
}

func newTestTracer(c *tracingCollector) *tracing.Tracer { return tracing.NewTracer(c) }
