package agents

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// llmHookRec records LLM lifecycle callbacks at both run and agent scope.
type llmHookRec struct {
	BaseRunHooks
	order       []string
	sysPrompt   string
	startInputN int
	endResp     *ModelResponse
	startErr    error
}

func (h *llmHookRec) OnLLMStart(_ context.Context, _ *RunContext, _ *Agent, sp string, input []TResponseInputItem) error {
	h.order = append(h.order, "start")
	h.sysPrompt = sp
	h.startInputN = len(input)
	return h.startErr
}

func (h *llmHookRec) OnLLMEnd(_ context.Context, _ *RunContext, _ *Agent, resp *ModelResponse) error {
	h.order = append(h.order, "end")
	h.endResp = resp
	return nil
}

func TestLLMHooks_FireAroundModelCall(t *testing.T) {
	hooks := &llmHookRec{}
	agent := &Agent{Name: "a", Instructions: StaticInstructions("sys prompt")}
	model := &fakeModel{responses: []*ModelResponse{{ResponseID: "r1"}}}

	_, err := Run(context.Background(), agent, "hello", RunOptions{Model: model, Hooks: hooks})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(hooks.order, ",") != "start,end" {
		t.Errorf("order = %v, want [start end]", hooks.order)
	}
	if hooks.sysPrompt != "sys prompt" {
		t.Errorf("systemPrompt = %q", hooks.sysPrompt)
	}
	if hooks.startInputN == 0 {
		t.Error("OnLLMStart received empty input")
	}
	if hooks.endResp == nil || hooks.endResp.ResponseID != "r1" {
		t.Errorf("OnLLMEnd response = %+v", hooks.endResp)
	}
}

// agentLLMHookRec records LLM callbacks via agent-scoped hooks.
type agentLLMHookRec struct {
	BaseAgentHooks
	starts, ends int
}

func (h *agentLLMHookRec) OnLLMStart(context.Context, *RunContext, *Agent, string, []TResponseInputItem) error {
	h.starts++
	return nil
}

func (h *agentLLMHookRec) OnLLMEnd(context.Context, *RunContext, *Agent, *ModelResponse) error {
	h.ends++
	return nil
}

func TestLLMHooks_AgentScopedFire(t *testing.T) {
	ah := &agentLLMHookRec{}
	agent := &Agent{Name: "a", Instructions: StaticInstructions("x"), Hooks: ah}
	model := &fakeModel{responses: []*ModelResponse{{ResponseID: "r1"}}}

	if _, err := Run(context.Background(), agent, "hi", RunOptions{Model: model}); err != nil {
		t.Fatal(err)
	}
	if ah.starts != 1 || ah.ends != 1 {
		t.Errorf("agent hooks starts=%d ends=%d, want 1 1", ah.starts, ah.ends)
	}
}

func TestLLMHooks_StartErrorAbortsRun(t *testing.T) {
	boom := errors.New("veto")
	hooks := &llmHookRec{startErr: boom}
	agent := &Agent{Name: "a", Instructions: StaticInstructions("x")}
	model := &fakeModel{responses: []*ModelResponse{{ResponseID: "r1"}}}

	_, err := Run(context.Background(), agent, "hi", RunOptions{Model: model, Hooks: hooks})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want veto", err)
	}
	if model.calls != 0 {
		t.Errorf("model called %d times, want 0 (OnLLMStart vetoed before the call)", model.calls)
	}
}

// The on_agent_end hook fires BEFORE output guardrails, so a tripped guardrail
// does not suppress it (Python parity).
func TestHooks_AgentEndFiresBeforeOutputGuardrail(t *testing.T) {
	var order []string
	agent := &Agent{
		Name:      "a",
		ModelImpl: &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "leak"))}},
		Guardrails: []Guardrail{{
			Name:   "pii",
			Stages: []GuardrailStage{StageOutput},
			Run: func(context.Context, *RunContext, GuardrailPayload) (GuardrailDecision, error) {
				order = append(order, "guardrail")
				return Trip(nil), nil
			},
		}},
	}
	hooks := &endOrderHooks{onEnd: func() { order = append(order, "agent_end") }}

	_, err := Run(context.Background(), agent, "hi", RunOptions{Hooks: hooks})
	var tw *GuardrailTripwireError
	if !errors.As(err, &tw) {
		t.Fatalf("want *GuardrailTripwireError, got %v", err)
	}
	if len(order) != 2 || order[0] != "agent_end" || order[1] != "guardrail" {
		t.Errorf("order = %v, want [agent_end guardrail]", order)
	}
}

type endOrderHooks struct {
	BaseRunHooks
	onEnd func()
}

func (h *endOrderHooks) OnAgentEnd(context.Context, *RunContext, *Agent, any) error {
	h.onEnd()
	return nil
}

// The agent-level OnHandoff fires on the SOURCE agent's hooks, not the
// target's (Python parity).
func TestHooks_AgentHandoffFiresOnSource(t *testing.T) {
	target := &Agent{
		Name:      "specialist",
		ModelImpl: &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "done"))}},
		Hooks:     &handoffHookRec{name: "target"},
	}
	sourceHooks := &handoffHookRec{name: "source"}
	source := &Agent{
		Name:      "triage",
		ModelImpl: &fakeModel{responses: []*ModelResponse{modelResp(functionCallOutput(t, "transfer_to_specialist", "h1", `{}`))}},
		Handoffs:  []Handoff{HandoffTo(target)},
		Hooks:     sourceHooks,
	}

	if _, err := Run(context.Background(), source, "go", RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if !sourceHooks.handoffFired {
		t.Error("source agent's OnHandoff did not fire")
	}
	if th, _ := target.Hooks.(*handoffHookRec); th != nil && th.handoffFired {
		t.Error("target agent's OnHandoff fired, but only the source's should")
	}
}

type handoffHookRec struct {
	BaseAgentHooks
	name         string
	handoffFired bool
	gotAgent     string
	gotSource    string
}

func (h *handoffHookRec) OnHandoff(_ context.Context, _ *RunContext, agent, source *Agent) error {
	h.handoffFired = true
	if agent != nil {
		h.gotAgent = agent.Name
	}
	if source != nil {
		h.gotSource = source.Name
	}
	return nil
}

// RunResult.NewItems is the unfiltered item log, so a handoff input
// filter that rewrites the model's view does not drop pre-handoff items from
// the result (Python parity: new_items = session_items).
func TestRunResult_NewItemsUnfilteredAfterHandoffFilter(t *testing.T) {
	target := &Agent{
		Name:      "specialist",
		ModelImpl: &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "handled"))}},
	}
	source := &Agent{
		Name:      "triage",
		ModelImpl: &fakeModel{responses: []*ModelResponse{modelResp(functionCallOutput(t, "transfer_to_specialist", "h1", `{}`))}},
		Handoffs:  []Handoff{HandoffTo(target)},
	}

	// NestHandoffHistory folds prior history into a summary, resetting the
	// model's generatedItems view on handoff.
	res, err := Run(context.Background(), source, "go", RunOptions{
		HandoffInputFilter: NestHandoffHistory(NestHistoryOptions{}),
	})
	if err != nil {
		t.Fatal(err)
	}
	// The result must still include the pre-handoff items (the handoff call and
	// its output) plus the target's final message — not just the post-handoff
	// items.
	var haveHandoffCall, haveFinalMessage bool
	for _, it := range res.NewItems {
		switch v := it.(type) {
		case *HandoffCallItem:
			haveHandoffCall = true
		case *MessageOutputItem:
			if v.Text() == "handled" {
				haveFinalMessage = true
			}
		}
	}
	if !haveHandoffCall {
		t.Error("NewItems dropped the pre-handoff handoff call (filtered view leaked into the result)")
	}
	if !haveFinalMessage {
		t.Error("NewItems missing the target agent's final message")
	}
}
