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
