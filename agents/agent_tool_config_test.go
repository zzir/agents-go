package agents

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/zzir/agents-go/agents/session"
)

// orchestratorCalling returns an orchestrator agent whose scripted model calls
// the given tool once and then finishes.
func orchestratorCalling(t *testing.T, tool *Tool, toolName, args string) *Agent {
	t.Helper()
	m := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, toolName, "c1", args)),
		modelResp(messageOutput(t, "orch done")),
	}}
	return &Agent{Name: "orchestrator", Tools: []*Tool{tool}, ModelImpl: m}
}

func TestAgentToolSessionPassthrough(t *testing.T) {
	sess := session.NewInMemorySession()
	sub := &Agent{Name: "specialist", ModelImpl: &fakeModel{responses: []*ModelResponse{
		modelResp(messageOutput(t, "nested answer")),
	}}}
	tool := sub.AsTool(AgentToolConfig{Name: "specialist", ModifyRunOptions: func(o *RunOptions) {
		o.Conversation.Session = sess
	}})
	orch := orchestratorCalling(t, tool, "specialist", `{"input":"remember me"}`)

	if _, err := RunSync(context.Background(), orch, "go", RunOptions{}); err != nil {
		t.Fatal(err)
	}
	items, err := session.NewSession(sess).ContextItems(context.Background(), session.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) == 0 {
		t.Fatal("nested run did not persist to the configured session")
	}
}

func TestAgentToolNeedsApprovalGatesParentRun(t *testing.T) {
	sub := &Agent{Name: "specialist", ModelImpl: &fakeModel{responses: []*ModelResponse{
		modelResp(messageOutput(t, "nested answer")),
	}}}
	tool := sub.AsTool(AgentToolConfig{Name: "specialist", NeedsApproval: true})
	orch := orchestratorCalling(t, tool, "specialist", `{"input":"hi"}`)

	res, err := RunSync(context.Background(), orch, "go", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Interruptions) != 1 || res.Interruptions[0].ToolName != "specialist" {
		t.Fatalf("interruptions = %+v, want the agent tool itself", res.Interruptions)
	}
	res.State.Approve(res.Interruptions[0], false)
	res2, err := ResumeRunSync(context.Background(), res.State, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res2.FinalOutputString() != "orch done" {
		t.Errorf("final after approval = %q", res2.FinalOutputString())
	}
}

func TestAgentToolInheritsRunLevelInputGuardrails(t *testing.T) {
	nestedRuns := 0
	sub := &Agent{Name: "specialist", ModelImpl: &fakeModel{responses: []*ModelResponse{
		modelResp(messageOutput(t, "nested answer")),
	}}}
	tool := sub.AsTool(AgentToolConfig{Name: "specialist"})
	orch := orchestratorCalling(t, tool, "specialist", `{"input":"BLOCKED payload"}`)

	guardCalls := 0
	guard := Guardrail{
		Name:     "no-blocked",
		Stages:   []GuardrailStage{StageInput},
		Blocking: true,
		Run: func(_ context.Context, _ *RunContext, p GuardrailPayload) (GuardrailDecision, error) {
			guardCalls++
			for _, it := range p.Input {
				b, _ := session.MarshalInputItem(it)
				if strings.Contains(string(b), "BLOCKED") {
					return Trip(nil), nil
				}
			}
			return Allow(nil), nil
		},
	}
	_ = nestedRuns

	res, err := RunSync(context.Background(), orch, "go", RunOptions{Guardrails: []Guardrail{guard}})
	if err != nil {
		t.Fatal(err)
	}
	// The parent input ("go") passes; the nested input ("BLOCKED payload")
	// trips the inherited run-level guardrail, so the nested run fails and the
	// error is fed back to the orchestrator as the tool output.
	if guardCalls < 2 {
		t.Fatalf("guardrail ran %d times, want parent + nested", guardCalls)
	}
	out := lastToolOutputText(t, res)
	if !strings.Contains(out, "tripwire") && !strings.Contains(out, "guardrail") {
		t.Errorf("tool output = %q, want nested guardrail failure fed back", out)
	}
}

// lastToolOutputText extracts the last tool output string from the run items.
func lastToolOutputText(t *testing.T, res *RunResult) string {
	t.Helper()
	for _, it := range slices.Backward(res.NewItems) {
		if it.Kind == ItemToolCallOutput {
			if s, ok := it.Output.(string); ok {
				return s
			}
		}
	}
	t.Fatal("no tool output item found")
	return ""
}

func TestAgentToolModifyRunOptions(t *testing.T) {
	// The nested agent wants two turns (tool call then answer); MaxTurns=1 via
	// ModifyRunOptions forces MaxTurnsExceeded inside the nested run.
	innerTool := NewTool("noop", "", func(context.Context, *ToolContext, struct{}) (string, error) {
		return "ok", nil
	})
	sub := &Agent{Name: "specialist", Tools: []*Tool{innerTool}, ModelImpl: &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "noop", "n1", `{}`)),
		modelResp(messageOutput(t, "never reached")),
	}}}
	tool := sub.AsTool(AgentToolConfig{
		Name:             "specialist",
		ModifyRunOptions: func(o *RunOptions) { o.Exec.MaxTurns = 1 },
	})
	orch := orchestratorCalling(t, tool, "specialist", `{"input":"hi"}`)

	res, err := RunSync(context.Background(), orch, "go", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	out := lastToolOutputText(t, res)
	if !strings.Contains(strings.ToLower(out), "max turns") && !strings.Contains(out, "MaxTurns") {
		t.Errorf("tool output = %q, want nested max-turns failure fed back", out)
	}
}

func TestMirrorIntoPreservesAlwaysDecisions(t *testing.T) {
	parent := NewApprovalStore()
	nested := NewApprovalStore()
	item := &ToolApprovalItem{ToolName: "danger", CallID: "call-1"}

	parent.Approve(item, true) // approve ALL future danger calls
	parent.mirrorInto(nested, []*ToolApprovalItem{item})

	// A different call id to the same tool must already be approved in the
	// nested store — permanence carried over.
	d, ok := nested.decisionFor("danger", "call-2")
	if !ok || !d.approved {
		t.Fatalf("always-approve did not stay permanent: ok=%v d=%+v", ok, d)
	}

	parent2 := NewApprovalStore()
	nested2 := NewApprovalStore()
	parent2.Reject(item, true, "never")
	parent2.mirrorInto(nested2, []*ToolApprovalItem{item})
	d2, ok2 := nested2.decisionFor("danger", "call-9")
	if !ok2 || d2.approved || d2.message != "never" {
		t.Fatalf("always-reject did not stay permanent: ok=%v d=%+v", ok2, d2)
	}
}

func TestAgentToolIsEnabledHidesTool(t *testing.T) {
	sub := &Agent{Name: "specialist", ModelImpl: &fakeModel{responses: []*ModelResponse{
		modelResp(messageOutput(t, "nested answer")),
	}}}
	tool := sub.AsTool(AgentToolConfig{
		Name:      "specialist",
		IsEnabled: func(context.Context, *RunContext, *Agent) (bool, error) { return false, nil },
	})
	m := &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "no tools"))}}
	orch := &Agent{Name: "orchestrator", Tools: []*Tool{tool}, ModelImpl: m}

	if _, err := RunSync(context.Background(), orch, "go", RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(m.lastReq.Tools) != 0 {
		t.Errorf("tools sent to model = %d, want 0 (disabled)", len(m.lastReq.Tools))
	}
}
