package agents

import (
	"context"
	"sync"
	"testing"

	"github.com/zzir/agents-go/tracing"
)

func approvalAgentAndModel(t *testing.T, ran *bool) *Agent {
	t.Helper()
	tool := NewTool("delete_db", "dangerous",
		func(ctx context.Context, tc *ToolContext, args struct{}) (string, error) {
			*ran = true
			return "deleted", nil
		})
	tool.NeedsApproval = true
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "delete_db", "call_1", `{}`)),
		modelResp(messageOutput(t, "all done")),
	}}
	return &Agent{Name: "a", Tools: []*Tool{tool}, ModelImpl: model}
}

func TestHITL_InterruptThenApprove(t *testing.T) {
	var ran bool
	agent := approvalAgentAndModel(t, &ran)

	res, err := RunSync(context.Background(), agent, "delete it", RunOptions{})
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
	res2, err := ResumeRunSync(context.Background(), res.State, RunOptions{})
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

// The pause snapshot must survive the resume it feeds: ResumeRun adopts a COPY
// of state.Usage, so the resumed run's accumulation cannot write through into
// the RunState the caller still holds. A Retry middleware resuming the same
// state twice would otherwise start its second attempt from the first attempt's
// inflated counters, and re-serializing the state after a resume would persist
// them.
func TestHITL_ResumeLeavesPauseStateUsageIntact(t *testing.T) {
	var ran bool
	agent := approvalAgentAndModel(t, &ran)

	res, err := RunSync(context.Background(), agent, "delete it", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	before := res.State.Usage.Snapshot()

	res.State.Approve(res.Interruptions[0], false)
	res2, err := ResumeRunSync(context.Background(), res.State, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// The resumed run billed its second model call on top of the carried
	// counters...
	if got := res2.Usage.Requests; got != before.Requests+1 {
		t.Errorf("resumed run Requests = %d, want %d", got, before.Requests+1)
	}
	// ...without touching the pause state it started from.
	after := res.State.Usage.Snapshot()
	if after.Requests != before.Requests || after.TotalTokens != before.TotalTokens {
		t.Errorf("pause state usage mutated by resume: before %d req / %d tok, after %d req / %d tok",
			before.Requests, before.TotalTokens, after.Requests, after.TotalTokens)
	}
}

func TestHITL_InterruptThenReject(t *testing.T) {
	var ran bool
	tool := NewTool("delete_db", "dangerous",
		func(ctx context.Context, tc *ToolContext, args struct{}) (string, error) {
			ran = true
			return "deleted", nil
		})
	tool.NeedsApproval = true
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "delete_db", "call_1", `{}`)),
		modelResp(messageOutput(t, "okay, skipped")),
	}}
	agent := &Agent{Name: "a", Tools: []*Tool{tool}, ModelImpl: model}

	res, err := RunSync(context.Background(), agent, "delete it", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	res.State.Reject(res.Interruptions[0], false, "denied by policy")
	res2, err := ResumeRunSync(context.Background(), res.State, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if ran {
		t.Error("tool should NOT run after rejection")
	}
	// The rejection message should appear as a tool output.
	var found bool
	for _, it := range res2.NewItems {
		if it.Kind == ItemToolCallOutput && it.Output == "denied by policy" {
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
	agent := approvalAgentAndModel(t, &ran)

	res, err := RunSync(context.Background(), agent, "delete it", RunOptions{})
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
	res2, err := ResumeRunSync(context.Background(), state, RunOptions{})
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

func TestHITL_ApproveToolsAgentLevel(t *testing.T) {
	var ran bool
	tool := NewTool("delete_db", "dangerous",
		func(ctx context.Context, tc *ToolContext, args struct{}) (string, error) {
			ran = true
			return "deleted", nil
		})
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "delete_db", "call_1", `{}`)),
		modelResp(messageOutput(t, "all done")),
	}}
	agent := &Agent{
		Name:         "a",
		Tools:        []*Tool{tool},
		ModelImpl:    model,
		ApproveTools: []string{"delete_db"},
	}

	res, err := RunSync(context.Background(), agent, "delete it", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Interruptions) != 1 {
		t.Fatalf("expected 1 interruption, got %d", len(res.Interruptions))
	}
	if ran {
		t.Error("tool should not have run before approval")
	}

	res.State.Approve(res.Interruptions[0], false)
	res2, err := ResumeRunSync(context.Background(), res.State, RunOptions{})
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

func TestHITL_ApproveToolsWildcard(t *testing.T) {
	tool := NewTool("any_tool", "does stuff",
		func(ctx context.Context, tc *ToolContext, args struct{}) (string, error) {
			return "ok", nil
		})
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "any_tool", "call_1", `{}`)),
		modelResp(messageOutput(t, "done")),
	}}
	agent := &Agent{
		Name:         "a",
		Tools:        []*Tool{tool},
		ModelImpl:    model,
		ApproveTools: []string{"*"},
	}

	res, err := RunSync(context.Background(), agent, "go", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Interruptions) != 1 {
		t.Fatalf("expected 1 interruption with wildcard, got %d", len(res.Interruptions))
	}
}

func TestHITL_UnknownAgentInRegistry(t *testing.T) {
	var ran bool
	agent := approvalAgentAndModel(t, &ran)
	res, _ := RunSync(context.Background(), agent, "go", RunOptions{})
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
	tool := NewTool("noop", "", func(ctx context.Context, tc *ToolContext, a struct{}) (string, error) {
		return "ok", nil
	})
	agent := &Agent{Name: "tracer-agent", Tools: []*Tool{tool}, ModelImpl: model}

	_, err := RunSync(context.Background(), agent, "go", RunOptions{Observe: ObserveOptions{Tracer: tr}})
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
		Guardrails: []Guardrail{{
			Name:   "g",
			Stages: []GuardrailStage{StageInput},
			Run: func(context.Context, *RunContext, GuardrailPayload) (GuardrailDecision, error) {
				return Allow(nil), nil
			},
		}},
	}

	if _, err := RunSync(context.Background(), src, "go", RunOptions{Observe: ObserveOptions{Tracer: newTestTracer(col)}}); err != nil {
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

// Approval precedence. A permanent approval wins over a later per-call
// rejection of the same tool.
func TestApprovalStore_Precedence(t *testing.T) {
	item := func(tool, callID string) *ToolApprovalItem {
		return &ToolApprovalItem{ToolName: tool, CallID: callID}
	}

	t.Run("permanent approve beats later per-call reject", func(t *testing.T) {
		s := NewApprovalStore()
		s.Approve(item("t", "c1"), true)       // always-approve t
		s.Reject(item("t", "c2"), false, "no") // reject a specific later call
		d, ok := s.decisionFor("t", "c2")
		if !ok || !d.approved {
			t.Errorf("c2 decision = %+v (ok=%v), want approved (permanent approval wins)", d, ok)
		}
	})

	t.Run("permanent reject beats per-call approve", func(t *testing.T) {
		s := NewApprovalStore()
		s.Reject(item("t", "c1"), true, "denied")
		s.Approve(item("t", "c2"), false)
		d, ok := s.decisionFor("t", "c2")
		if !ok || d.approved {
			t.Errorf("c2 decision = %+v (ok=%v), want rejected (permanent rejection wins)", d, ok)
		}
	})

	t.Run("per-call approve beats per-call reject on same call", func(t *testing.T) {
		s := NewApprovalStore()
		s.Reject(item("t", "c1"), false, "no")
		s.Approve(item("t", "c1"), false) // approve supersedes on the same call
		d, ok := s.decisionFor("t", "c1")
		if !ok || !d.approved {
			t.Errorf("c1 decision = %+v (ok=%v), want approved", d, ok)
		}
	})

	t.Run("undecided call of a partially-decided tool", func(t *testing.T) {
		s := NewApprovalStore()
		s.Approve(item("t", "c1"), false)
		if _, ok := s.decisionFor("t", "other"); ok {
			t.Error("an unrelated call should be undecided")
		}
	})
}

// The per-tool entry structure round-trips through RunState JSON.
func TestApprovalStore_SerializationRoundTrip(t *testing.T) {
	var ran bool
	agent := approvalAgentAndModel(t, &ran)
	res, err := RunSync(context.Background(), agent, "go", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Record a permanent approval plus a per-call rejection message.
	res.State.Approve(res.Interruptions[0], true)
	res.State.Reject(&ToolApprovalItem{ToolName: "delete_db", CallID: "other"}, false, "blocked")

	data, err := res.State.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	restored, err := RunStateFromJSON(data, map[string]*Agent{"a": agent})
	if err != nil {
		t.Fatal(err)
	}
	// Permanent approval survives and still wins over the per-call rejection.
	if d, ok := restored.Approvals.decisionFor("delete_db", "other"); !ok || !d.approved {
		t.Errorf("restored decision = %+v (ok=%v), want approved", d, ok)
	}
}
