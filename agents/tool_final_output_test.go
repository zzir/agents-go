package agents

import (
	"context"
	"strings"
	"testing"
)

// A terminating tool whose output is multimodal must reach the caller as the
// JSON the model was sent, not as Go's default struct rendering. The two ways
// the same item is read — RunResult.FinalOutput here, ItemDisplay.Output for a
// UI or a stored session — otherwise disagree about what the tool said.
func TestFinalOutput_MultimodalToolRendersAsJSON(t *testing.T) {
	tool := NewTool("chart", "", func(context.Context, *ToolContext, struct{}) (ToolResult, error) {
		return ToolResult{
			Content: []ToolOutputContent{
				ToolOutputText{Text: "see chart"},
				ToolOutputImageFromBytes("image/png", []byte{1, 2, 3}),
			},
			Terminate: true,
		}, nil
	})
	model := &fakeModel{responses: []*ModelResponse{modelResp(functionCallOutput(t, "chart", "c1", `{}`))}}
	agent := &Agent{Name: "a", Tools: []*Tool{tool}, ModelImpl: model}

	res, err := RunSync(context.Background(), agent, "go", RunOptions{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	final := res.FinalOutputString()
	if !strings.HasPrefix(final, `[{"`) {
		t.Errorf("final output = %q, want the JSON the model was sent", final)
	}
	if strings.Contains(final, "{see chart}") {
		t.Errorf("final output = %q — Go syntax, not JSON", final)
	}
	// Same value, same rendering, wherever it is read from.
	for _, it := range res.NewItems {
		if it.Kind == ItemToolCallOutput {
			if got := it.Display().Output; got != final {
				t.Errorf("display output = %q, final output = %q — the two renderings must agree", got, final)
			}
		}
	}
}
