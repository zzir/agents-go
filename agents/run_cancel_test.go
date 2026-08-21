package agents_test

import (
	"context"
	"errors"
	"testing"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/internal/agentstest"
)

// TestCancelAtTurnBoundaryCarriesProgress pins the shape a cancellation takes
// when it is noticed at a turn boundary rather than inside the model call: a
// *RunError carrying the turns that did complete, exactly like every other
// failure inside the loop. Returning ctx.Err() bare there — which is what the
// loop used to do — dropped the finished turn's items and made the same user
// action (a cancel) reach the caller in two different shapes depending on
// where the deadline landed.
func TestCancelAtTurnBoundaryCarriesProgress(t *testing.T) {
	model := agentstest.NewResponseBuilder().
		FunctionCall("ping", "call_1", `{}`).
		NewTurn().
		Text("never reached").
		Build()

	type pingArgs struct{}
	agent := &agents.Agent{
		Name:      "Canceller",
		ModelImpl: model,
		Tools: []*agents.Tool{
			agents.NewTool("ping", "ping", func(_ context.Context, _ *agents.ToolContext, _ pingArgs) (string, error) {
				return "pong", nil
			}),
		},
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// The turn completed, its tools ran and its items were recorded; the cancel
	// lands after the save point, so the next iteration's boundary check is
	// what sees it.
	opts := agents.RunOptions{Exec: agents.ExecOptions{
		PrepareNextTurn: func(context.Context, *agents.TurnResult) (*agents.TurnSnapshot, error) {
			cancel()
			return nil, nil
		},
	}}

	res, err := agents.RunSync(ctx, agent, "hi", opts)
	if res != nil {
		t.Fatalf("res = %v, want nil", res)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	runErr, ok := errors.AsType[*agents.RunError](err)
	if !ok {
		t.Fatalf("err = %T, want *agents.RunError wrapping the cancellation", err)
	}
	if runErr.Result == nil {
		t.Fatal("RunError.Result = nil, want the run's partial progress")
	}
	if got := agentstest.ItemTypes(runErr.Result.NewItems); len(got) == 0 {
		t.Fatalf("RunError.Result.NewItems is empty, want the completed turn's items")
	}
	if names := agentstest.ToolCallNames(runErr.Result.NewItems); len(names) != 1 || names[0] != "ping" {
		t.Errorf("tool calls = %v, want [ping]", names)
	}
	if runErr.Result.LastAgent != agent {
		t.Errorf("LastAgent = %v, want %q", runErr.Result.LastAgent, agent.Name)
	}
	if runErr.Result.FinalOutput != nil {
		t.Errorf("FinalOutput = %v, want nil", runErr.Result.FinalOutput)
	}
}
