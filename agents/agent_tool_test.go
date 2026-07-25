package agents

import (
	"context"
	"testing"
)

func TestAgentAsTool(t *testing.T) {
	// Sub-agent that summarizes; has its own model.
	subModel := &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "SUMMARY: hello"))}}
	sub := &Agent{Name: "summarizer", ModelImpl: subModel}
	summarize := sub.AsTool(AgentToolConfig{Name: "summarize", Description: "summarize text"})

	// Orchestrator calls the sub-agent tool, then produces a final answer.
	orchModel := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "summarize", "c1", `{"input":"some long text"}`)),
		modelResp(messageOutput(t, "done: SUMMARY: hello")),
	}}
	orch := &Agent{Name: "orchestrator", Tools: []Tool{summarize}, ModelImpl: orchModel}

	res, err := RunSync(context.Background(), orch, "summarize this", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.FinalOutputString() != "done: SUMMARY: hello" {
		t.Errorf("final = %q", res.FinalOutputString())
	}
	// The sub-agent's model must have been called with the forwarded input.
	if subModel.calls != 1 {
		t.Errorf("sub-agent model calls = %d, want 1", subModel.calls)
	}
}

func TestAgentAsTool_CustomExtractor(t *testing.T) {
	subModel := &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "raw"))}}
	sub := &Agent{Name: "s", ModelImpl: subModel}
	tool := sub.AsTool(AgentToolConfig{
		Name: "s",
		CustomOutputExtractor: func(r *RunResult) (string, error) {
			return "extracted:" + r.FinalOutputString(), nil
		},
	})
	ft := tool.(*FunctionTool)
	out, err := ft.OnInvoke(context.Background(), &ToolContext{RunContext: NewRunContext(nil)}, `{"input":"x"}`)
	if err != nil {
		t.Fatal(err)
	}
	if out.ModelOutput() != "extracted:raw" {
		t.Errorf("out = %v", out)
	}
}
