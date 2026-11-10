package agents_test

import (
	"context"
	"testing"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/internal/agentstest"
)

// pingAgent is a two-turn script: call the ping tool, then answer. It gives the
// turn-boundary hooks a turn with items in it to look at.
func pingAgent() *agents.Agent {
	type pingArgs struct{}
	return &agents.Agent{
		Name: "Pinger",
		ModelImpl: agentstest.NewResponseBuilder().
			FunctionCall("ping", "call_1", `{}`).
			NewTurn().
			Text("done").
			Build(),
		Tools: []*agents.Tool{
			agents.NewTool("ping", "ping", func(_ context.Context, _ *agents.ToolContext, _ pingArgs) (string, error) {
				return "pong", nil
			}),
		},
	}
}

// TestStopAfterTurnFinalOutputIgnoresHookWrites is the same isolation seen from
// the other side: the final output of a run stopped at a turn boundary is
// derived from the turn that happened, not from whatever the hook left behind
// in the value it was handed.
func TestStopAfterTurnFinalOutputIgnoresHookWrites(t *testing.T) {
	opts := agents.RunOptions{Exec: agents.ExecOptions{
		ShouldStopAfterTurn: func(_ context.Context, tr *agents.TurnResult) (bool, error) {
			tr.NewItems = nil
			return true, nil
		},
	}}

	res, err := agents.RunSync(t.Context(), pingAgent(), "hi", opts)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	// The turn ran a tool and never got to write a closing message, so the tool
	// output is the final output.
	if res.FinalOutput != "pong" {
		t.Errorf("FinalOutput = %v, want %q", res.FinalOutput, "pong")
	}
	if len(res.NewItems) == 0 {
		t.Errorf("NewItems is empty, want the stopped turn's items")
	}
}
