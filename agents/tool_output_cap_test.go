package agents_test

import (
	"context"
	"strings"
	"testing"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/internal/agentstest"
)

// runOneTool runs a tool that returns out and hands back the text the model
// was sent for it.
func runOneTool(t *testing.T, out any, exec agents.ExecOptions) string {
	t.Helper()
	tool := agents.NewTool("big", "returns a lot",
		func(context.Context, *agents.ToolContext, struct{}) (any, error) { return out, nil })
	model := agentstest.NewResponseBuilder().
		FunctionCall("big", "call-1", "{}").
		NewTurn().
		Text("ok").
		Build()
	agent := &agents.Agent{Name: "a", ModelImpl: model, Tools: []*agents.Tool{tool}}
	res, err := agents.RunSync(context.Background(), agent, "go", agents.RunOptions{Exec: exec})
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range res.NewItems {
		if it.Kind == agents.ItemToolCallOutput {
			return it.Display().Output
		}
	}
	t.Fatal("no tool output item")
	return ""
}

// A text result past the limit reaches the model with its head and tail and
// a marker in between; the default is 64 KiB, a negative limit turns it off.
func TestToolOutputIsCappedAtTheRunner(t *testing.T) {
	big := strings.Repeat("h", 100<<10) + "MIDDLE" + strings.Repeat("t", 100<<10)
	got := runOneTool(t, big, agents.ExecOptions{})
	if len(got) > agents.DefaultToolOutputLimit+64 {
		t.Fatalf("sent %d bytes, want about %d", len(got), agents.DefaultToolOutputLimit)
	}
	if !strings.HasPrefix(got, "hhhh") || !strings.HasSuffix(got, "tttt") || !strings.Contains(got, "[omitted ") || strings.Contains(got, "MIDDLE") {
		t.Fatalf("elided text = %.40q…%.40q", got, got[len(got)-40:])
	}
	if full := runOneTool(t, big, agents.ExecOptions{ToolOutputLimit: -1}); full != big {
		t.Fatalf("a negative limit still capped: %d bytes", len(full))
	}
	if small := runOneTool(t, "short", agents.ExecOptions{ToolOutputLimit: 16}); small != "short" {
		t.Fatalf("a result under the limit changed: %q", small)
	}
}

// Only the text parts of a multimodal result are elided; an image part rides through.
func TestToolOutputCapKeepsImageParts(t *testing.T) {
	out := []agents.ToolOutputContent{
		agents.ToolOutputText{Text: strings.Repeat("x", 4096)},
		agents.ToolOutputImage{ImageURL: "https://example.test/i.png"},
	}
	got := runOneTool(t, out, agents.ExecOptions{ToolOutputLimit: 1024})
	if !strings.Contains(got, "[omitted ") || !strings.Contains(got, "example.test/i.png") {
		t.Fatalf("multimodal output = %.200q", got)
	}
}
