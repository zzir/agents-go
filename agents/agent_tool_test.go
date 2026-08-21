package agents

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/zzir/agents-go/agents/session"
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
	orch := &Agent{Name: "orchestrator", Tools: []*Tool{summarize}, ModelImpl: orchModel}

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

// TestAgentAsTool_ValidatesDefaultInput pins the default {"input": string}
// arguments to the schema the tool advertises: anything else is a
// *ModelBehaviorError the calling model can correct, never the nested agent's
// prompt.
func TestAgentAsTool_ValidatesDefaultInput(t *testing.T) {
	for _, tc := range []struct {
		name string
		args string
		want string // the nested run's input; empty means the call must be rejected
	}{
		{name: "empty object", args: `{}`},
		{name: "wrong key", args: `{"query":"find pandas"}`},
		{name: "wrong type", args: `{"input":42}`},
		{name: "not an object", args: `"hi"`},
		{name: "not json", args: `hi`},
		{name: "valid", args: `{"input":"summarize this"}`, want: "summarize this"},
		{name: "valid with extra key", args: `{"input":"summarize this","extra":1}`, want: "summarize this"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			subModel := &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "SUMMARY"))}}
			sub := &Agent{Name: "summarizer", ModelImpl: subModel}
			tool := sub.AsTool(AgentToolConfig{Name: "summarize"})

			tCtx := &ToolContext{RunContext: NewRunContext(nil)}
			_, err := tool.OnInvoke(context.Background(), tCtx, tc.args)
			if tc.want == "" {
				if _, ok := errors.AsType[*ModelBehaviorError](err); !ok {
					t.Fatalf("err = %v (%T), want *ModelBehaviorError", err, err)
				}
				if subModel.calls != 0 {
					t.Fatalf("nested agent ran on invalid arguments: %v", subModel.lastReq.Input)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			item, merr := session.MarshalInputItem(subModel.lastReq.Input[len(subModel.lastReq.Input)-1])
			if merr != nil {
				t.Fatal(merr)
			}
			if nested := string(item); !strings.Contains(nested, tc.want) || strings.Contains(nested, `\"input\"`) {
				t.Errorf("nested input = %q, want the bare %q", nested, tc.want)
			}
		})
	}
}

// TestAsTool_CustomInputBuilderValidatesInput pins the same check on AsTool's
// other path: an InputBuilder replaces the rendering, not the {"input": string}
// schema the tool still advertises, so the builder never sees arguments that
// violate it.
func TestAsTool_CustomInputBuilderValidatesInput(t *testing.T) {
	subModel := &fakeModel{responses: []*ModelResponse{modelResp(messageOutput(t, "ANSWER"))}}
	sub := &Agent{Name: "helper", ModelImpl: subModel}
	tool := sub.AsTool(AgentToolConfig{
		Name: "ask",
		InputBuilder: func(opts AgentToolInputBuilderOptions) (string, error) {
			return "CUSTOM:" + opts.ParamsJSON, nil
		},
	})
	tCtx := &ToolContext{RunContext: NewRunContext(nil)}

	_, err := tool.OnInvoke(context.Background(), tCtx, `{"q":"cats","n":3}`)
	if _, ok := errors.AsType[*ModelBehaviorError](err); !ok {
		t.Fatalf("err = %v (%T), want *ModelBehaviorError", err, err)
	}
	if subModel.calls != 0 {
		t.Fatalf("nested agent ran on invalid arguments: %v", subModel.lastReq.Input)
	}

	if _, err := tool.OnInvoke(context.Background(), tCtx, `{"input":"hi"}`); err != nil {
		t.Fatal(err)
	}
	item, merr := session.MarshalInputItem(subModel.lastReq.Input[len(subModel.lastReq.Input)-1])
	if merr != nil {
		t.Fatal(merr)
	}
	if nested := string(item); !strings.Contains(nested, "CUSTOM:") {
		t.Errorf("nested input = %q, want the custom builder's rendering", nested)
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
	ft := tool
	out, err := ft.OnInvoke(context.Background(), &ToolContext{RunContext: NewRunContext(nil)}, `{"input":"x"}`)
	if err != nil {
		t.Fatal(err)
	}
	if out.ModelOutput() != "extracted:raw" {
		t.Errorf("out = %v", out)
	}
}
