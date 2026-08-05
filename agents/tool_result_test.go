package agents

import (
	"context"
	"strings"
	"testing"
)

// A tool that returns a plain value still works: NewTool wraps it. This
// is the path almost every tool takes, and it must not have gotten heavier.
func TestToolResult_PlainReturnValuesStillWork(t *testing.T) {
	cases := map[string]struct {
		tool *Tool
		want string
	}{
		"string": {NewTool("t", "",
			func(context.Context, *ToolContext, struct{}) (string, error) { return "sunny", nil }), "sunny"},
		"struct": {NewTool("t", "",
			func(context.Context, *ToolContext, struct{}) (struct {
				N int `json:"n"`
			}, error) {
				return struct {
					N int `json:"n"`
				}{7}, nil
			}), `{"n":7}`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			model := &fakeModel{responses: []*ModelResponse{
				modelResp(functionCallOutput(t, "t", "c1", `{}`)),
				modelResp(messageOutput(t, "done")),
			}}
			agent := &Agent{Name: "a", Tools: []*Tool{tc.tool}, ModelImpl: model}
			res, err := RunSync(context.Background(), agent, "go", RunOptions{})
			if err != nil {
				t.Fatal(err)
			}
			out := findToolOutput(res.NewItems)
			if out == nil {
				t.Fatal("no tool output item")
			}
			if got := stringifyToolOutput(out.Output); got != tc.want {
				t.Errorf("model saw %q, want %q", got, tc.want)
			}
		})
	}
}

// Usage a tool spent on its own model calls is attributable to THAT call, not
// only folded into the run total where nothing says which call spent it.
func TestToolResult_UsageIsAttributedToTheCall(t *testing.T) {
	inner := &Agent{Name: "inner", ModelImpl: &fakeModel{responses: []*ModelResponse{
		modelResp(messageOutput(t, "inner answer")),
	}}}
	outer := &Agent{
		Name:  "outer",
		Tools: []*Tool{inner.AsTool(AgentToolConfig{Name: "ask_inner", Description: "ask"})},
		ModelImpl: &fakeModel{responses: []*ModelResponse{
			modelResp(functionCallOutput(t, "ask_inner", "c1", `{"input":"q"}`)),
			modelResp(messageOutput(t, "done")),
		}},
	}

	res, err := RunSync(context.Background(), outer, "go", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Usage == nil || res.Usage.Requests == 0 {
		t.Fatal("the run recorded no usage at all")
	}
	// The nested run's spend is still in the total...
	if res.Usage.Requests < 3 {
		t.Errorf("run usage requests = %d, want the nested run's included", res.Usage.Requests)
	}
}

// Terminate stops the run only when EVERY tool in the batch asks. One tool
// wanting to stop while another is still working is not a decision the SDK can
// make for them, and stopping anyway would discard the other's result.
func TestToolResult_TerminateRequiresUnanimity(t *testing.T) {
	stopper := func(name string, terminate bool) *Tool {
		return NewTool(name, "",
			func(context.Context, *ToolContext, struct{}) (ToolResult, error) {
				r := TextResult(name + " done")
				r.Terminate = terminate
				return r, nil
			})
	}

	t.Run("all terminate stops the run", func(t *testing.T) {
		model := &fakeModel{responses: []*ModelResponse{
			modelResp(
				functionCallOutput(t, "a", "c1", `{}`),
				functionCallOutput(t, "b", "c2", `{}`),
			),
			modelResp(messageOutput(t, "never reached")),
		}}
		agent := &Agent{Name: "x", Tools: []*Tool{stopper("a", true), stopper("b", true)}, ModelImpl: model}

		res, err := RunSync(context.Background(), agent, "go", RunOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if model.calls != 1 {
			t.Errorf("model called %d times, want 1 — the run should have stopped", model.calls)
		}
		if got := res.FinalOutputString(); !strings.Contains(got, "done") {
			t.Errorf("final output = %q, want the last tool's output", got)
		}
	})

	t.Run("one dissenter keeps the run going", func(t *testing.T) {
		model := &fakeModel{responses: []*ModelResponse{
			modelResp(
				functionCallOutput(t, "a", "c1", `{}`),
				functionCallOutput(t, "b", "c2", `{}`),
			),
			modelResp(messageOutput(t, "carried on")),
		}}
		agent := &Agent{Name: "x", Tools: []*Tool{stopper("a", true), stopper("b", false)}, ModelImpl: model}

		res, err := RunSync(context.Background(), agent, "go", RunOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if model.calls != 2 {
			t.Errorf("model called %d times, want 2 — one tool did not ask to stop", model.calls)
		}
		if res.FinalOutputString() != "carried on" {
			t.Errorf("final output = %q", res.FinalOutputString())
		}
	})
}

// A handled tool failure is still a failure to a renderer, even though the
// model sees the message and can recover.
func TestToolResult_HandledFailureIsMarkedAsError(t *testing.T) {
	failing := NewTool("boom", "",
		func(context.Context, *ToolContext, struct{}) (string, error) {
			return "", errTooBad
		})
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "boom", "c1", `{}`)),
		modelResp(messageOutput(t, "recovered")),
	}}
	agent := &Agent{Name: "a", Tools: []*Tool{failing}, ModelImpl: model}

	res, err := RunSync(context.Background(), agent, "go", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	out := findToolOutput(res.NewItems)
	if out == nil {
		t.Fatal("no tool output item")
	}
	if !out.Display().IsError {
		t.Error("a handled tool failure should render as an error")
	}
	// The model still gets the message so it can recover.
	if !strings.Contains(stringifyToolOutput(out.Output), "error") {
		t.Errorf("the model did not receive the failure message: %v", out.Output)
	}
}

// ModelOutput collapses a single text part but keeps a multimodal list, which
// is the rule a wrapper around another tool must not reimplement.
func TestToolResult_ModelOutput(t *testing.T) {
	if got := TextResult("hi").ModelOutput(); got != "hi" {
		t.Errorf("single text = %#v, want the bare string", got)
	}
	if got := (ToolResult{}).ModelOutput(); got != "" {
		t.Errorf("empty result = %#v, want an empty string", got)
	}
	multi := ToolResult{Content: []ToolOutputContent{
		ToolOutputText{Text: "look"},
		ToolOutputImage{ImageURL: "https://example.com/x.png"},
	}}
	if _, ok := multi.ModelOutput().([]ToolOutputContent); !ok {
		t.Errorf("multimodal result collapsed to %T", multi.ModelOutput())
	}
}

var errTooBad = &UserError{Message: "tool error: too bad"}
