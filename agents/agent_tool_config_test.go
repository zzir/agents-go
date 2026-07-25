package agents

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// orchestratorCalling returns an orchestrator agent whose scripted model calls
// the given tool once and then finishes.
func orchestratorCalling(t *testing.T, tool Tool, toolName, args string) *Agent {
	t.Helper()
	m := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, toolName, "c1", args)),
		modelResp(messageOutput(t, "orch done")),
	}}
	return &Agent{Name: "orchestrator", Tools: []Tool{tool}, ModelImpl: m}
}

func TestAgentToolOnStreamDeliversEvents(t *testing.T) {
	sub := &Agent{Name: "specialist", ModelImpl: &fakeModel{responses: []*ModelResponse{
		modelResp(messageOutput(t, "nested answer")),
	}}}

	var mu sync.Mutex
	var events []AgentToolStreamEvent
	tool := sub.AsTool(AgentToolConfig{
		Name: "specialist",
		OnStream: func(ev AgentToolStreamEvent) {
			mu.Lock()
			events = append(events, ev)
			mu.Unlock()
		},
	})
	orch := orchestratorCalling(t, tool, "specialist", `{"input":"hi"}`)

	res, err := RunSync(context.Background(), orch, "go", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.FinalOutputString() != "orch done" {
		t.Fatalf("final = %q", res.FinalOutputString())
	}

	mu.Lock()
	defer mu.Unlock()
	if len(events) == 0 {
		t.Fatal("no OnStream events delivered")
	}
	var sawRaw, sawItem bool
	for _, ev := range events {
		if ev.Agent == nil || ev.Agent.Name != "specialist" {
			t.Fatalf("event agent = %+v, want specialist", ev.Agent)
		}
		if ev.ToolCallID != "c1" || ev.ToolName != "specialist" {
			t.Fatalf("event call metadata = %q/%q", ev.ToolCallID, ev.ToolName)
		}
		switch ev.Event.(type) {
		case *RawResponsesStreamEvent:
			sawRaw = true
		case *RunItemStreamEvent:
			sawItem = true
		}
	}
	if !sawRaw || !sawItem {
		t.Errorf("event kinds: raw=%v item=%v, want both", sawRaw, sawItem)
	}
}

func TestAgentToolOnStreamHandlerPanicDoesNotFailCall(t *testing.T) {
	sub := &Agent{Name: "specialist", ModelImpl: &fakeModel{responses: []*ModelResponse{
		modelResp(messageOutput(t, "nested answer")),
	}}}
	tool := sub.AsTool(AgentToolConfig{
		Name:     "specialist",
		OnStream: func(AgentToolStreamEvent) { panic("handler bug") },
	})
	orch := orchestratorCalling(t, tool, "specialist", `{"input":"hi"}`)

	res, err := RunSync(context.Background(), orch, "go", RunOptions{})
	if err != nil {
		t.Fatalf("handler panic must not fail the run: %v", err)
	}
	if res.FinalOutputString() != "orch done" {
		t.Errorf("final = %q", res.FinalOutputString())
	}
}

func TestAgentToolSessionPassthrough(t *testing.T) {
	sess := NewInMemorySession()
	sub := &Agent{Name: "specialist", ModelImpl: &fakeModel{responses: []*ModelResponse{
		modelResp(messageOutput(t, "nested answer")),
	}}}
	tool := sub.AsTool(AgentToolConfig{Name: "specialist", Session: sess})
	orch := orchestratorCalling(t, tool, "specialist", `{"input":"remember me"}`)

	if _, err := RunSync(context.Background(), orch, "go", RunOptions{}); err != nil {
		t.Fatal(err)
	}
	items, err := SessionItems(context.Background(), sess, 0)
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
				b, _ := MarshalInputItem(it)
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
	for i := len(res.NewItems) - 1; i >= 0; i-- {
		if it, ok := res.NewItems[i].(*ToolCallOutputItem); ok {
			if s, ok := it.Output.(string); ok {
				return s
			}
		}
	}
	t.Fatal("no tool output item found")
	return ""
}

func TestAgentToolInvocationExposedToExtractor(t *testing.T) {
	sub := &Agent{Name: "specialist", ModelImpl: &fakeModel{responses: []*ModelResponse{
		modelResp(messageOutput(t, "nested answer")),
	}}}
	var got *AgentToolInvocation
	tool := sub.AsTool(AgentToolConfig{
		Name: "specialist",
		CustomOutputExtractor: func(res *RunResult) (string, error) {
			got = res.AgentToolInvocation
			return "extracted", nil
		},
	})
	orch := orchestratorCalling(t, tool, "specialist", `{"input":"hi"}`)

	if _, err := RunSync(context.Background(), orch, "go", RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ToolName != "specialist" || got.ToolCallID != "c1" {
		t.Fatalf("AgentToolInvocation = %+v", got)
	}
	if !strings.Contains(got.Arguments, "hi") {
		t.Errorf("Arguments = %q", got.Arguments)
	}
}

func TestAgentToolModifyRunOptions(t *testing.T) {
	// The nested agent wants two turns (tool call then answer); MaxTurns=1 via
	// ModifyRunOptions forces MaxTurnsExceeded inside the nested run.
	innerTool := NewFunctionTool("noop", "", func(context.Context, *ToolContext, struct{}) (string, error) {
		return "ok", nil
	})
	sub := &Agent{Name: "specialist", Tools: []Tool{innerTool}, ModelImpl: &fakeModel{responses: []*ModelResponse{
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

type asToolParams struct {
	Query string `json:"query" jsonschema:"The search query"`
	Limit int64  `json:"limit" jsonschema:"Max results"`
}

func TestAgentAsToolStructuredParams(t *testing.T) {
	subModel := &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "found"))}}
	sub := &Agent{Name: "searcher", ModelImpl: subModel}
	tool := AgentAsTool[asToolParams](sub, AgentToolConfig{Name: "search", Description: "search things"})

	ft := tool.(*FunctionTool)
	props, _ := ft.ParamsJSONSchema["properties"].(map[string]any)
	if _, ok := props["query"]; !ok {
		t.Fatalf("schema properties = %v, want query field", props)
	}

	orch := orchestratorCalling(t, tool, "search", `{"query":"cats","limit":3}`)
	if _, err := RunSync(context.Background(), orch, "go", RunOptions{}); err != nil {
		t.Fatal(err)
	}
	// The nested agent receives the default structured rendering.
	lastItem, _ := MarshalInputItem(subModel.lastReq.Input[len(subModel.lastReq.Input)-1])
	nestedInput := string(lastItem)
	if !strings.Contains(nestedInput, "called as a tool") || !strings.Contains(nestedInput, "cats") {
		t.Errorf("nested input = %q, want structured preamble + params", nestedInput)
	}
	if !strings.Contains(nestedInput, "Input Schema Summary") {
		t.Errorf("nested input = %q, want schema summary section", nestedInput)
	}
}

// Params that carry no descriptions anywhere: the schema summary is empty,
// but the rendering must still be structural (preamble + JSON) — and the
// arguments must still decode into Params before reaching the nested run.
type asToolBareParams struct {
	Query string `json:"query"`
	Limit int64  `json:"limit"`
}

func TestAgentAsToolValidatesParams(t *testing.T) {
	subModel := &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "found"))}}
	sub := &Agent{Name: "searcher", ModelImpl: subModel}
	tool := AgentAsTool[asToolParams](sub, AgentToolConfig{Name: "search"})

	// A type mismatch (limit as string) must bounce back to the model as a
	// tool error, not flow into the nested run.
	orch := orchestratorCalling(t, tool, "search", `{"query":"cats","limit":"three"}`)
	res, err := RunSync(context.Background(), orch, "go", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if subModel.calls != 0 {
		t.Fatalf("nested agent ran despite invalid params: %v", subModel.lastReq.Input)
	}
	if out := res.FinalOutputString(); out == "" {
		t.Fatal("expected the orchestrator to receive the tool error and answer")
	}
}

func TestAgentAsToolStructuredWithoutDescriptions(t *testing.T) {
	subModel := &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "found"))}}
	sub := &Agent{Name: "searcher", ModelImpl: subModel}
	tool := AgentAsTool[asToolBareParams](sub, AgentToolConfig{Name: "search"})

	orch := orchestratorCalling(t, tool, "search", `{"query":"cats","limit":3}`)
	if _, err := RunSync(context.Background(), orch, "go", RunOptions{}); err != nil {
		t.Fatal(err)
	}
	// No descriptions -> no schema summary, but the structured preamble and
	// the JSON arguments must still be rendered (not raw passthrough).
	lastItem, _ := MarshalInputItem(subModel.lastReq.Input[len(subModel.lastReq.Input)-1])
	nestedInput := string(lastItem)
	if !strings.Contains(nestedInput, "called as a tool") || !strings.Contains(nestedInput, "cats") {
		t.Errorf("nested input = %q, want structured preamble + params", nestedInput)
	}
	if strings.Contains(nestedInput, "Input Schema Summary") {
		t.Errorf("nested input = %q, unexpected summary section for description-less params", nestedInput)
	}
}

func TestAgentAsToolIncludeInputSchema(t *testing.T) {
	subModel := &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "found"))}}
	sub := &Agent{Name: "searcher", ModelImpl: subModel}
	tool := AgentAsTool[asToolParams](sub, AgentToolConfig{Name: "search", IncludeInputSchema: true})

	orch := orchestratorCalling(t, tool, "search", `{"query":"cats","limit":3}`)
	if _, err := RunSync(context.Background(), orch, "go", RunOptions{}); err != nil {
		t.Fatal(err)
	}
	lastItem, _ := MarshalInputItem(subModel.lastReq.Input[len(subModel.lastReq.Input)-1])
	nestedInput := string(lastItem)
	if !strings.Contains(nestedInput, "Input JSON Schema") {
		t.Errorf("nested input = %q, want full JSON schema section", nestedInput)
	}
}

func TestAgentAsToolCustomInputBuilder(t *testing.T) {
	subModel := &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "found"))}}
	sub := &Agent{Name: "searcher", ModelImpl: subModel}
	tool := AgentAsTool[asToolParams](sub, AgentToolConfig{
		Name: "search",
		InputBuilder: func(opts AgentToolInputBuilderOptions) (string, error) {
			return "CUSTOM:" + opts.ParamsJSON, nil
		},
	})
	orch := orchestratorCalling(t, tool, "search", `{"query":"cats","limit":3}`)
	if _, err := RunSync(context.Background(), orch, "go", RunOptions{}); err != nil {
		t.Fatal(err)
	}
	lastItem, _ := MarshalInputItem(subModel.lastReq.Input[len(subModel.lastReq.Input)-1])
	nestedInput := string(lastItem)
	if !strings.Contains(nestedInput, "CUSTOM:") {
		t.Errorf("nested input = %q, want custom builder output", nestedInput)
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
	orch := &Agent{Name: "orchestrator", Tools: []Tool{tool}, ModelImpl: m}

	if _, err := RunSync(context.Background(), orch, "go", RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(m.lastReq.Tools) != 0 {
		t.Errorf("tools sent to model = %d, want 0 (disabled)", len(m.lastReq.Tools))
	}
}
