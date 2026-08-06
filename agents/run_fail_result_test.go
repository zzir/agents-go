package agents_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agentstest"
)

// TestFailAfterHandoffFilterReportsUnfilteredLog pins what a failed run reports
// as its items when a handoff input filter has already thrown the model's view
// away.
//
// The filter rewrites what the NEXT agent is sent; it never rewrites what the
// run produced. The loop implements that by resetting its generated-items list
// to nothing while the session log keeps everything, so a failure right after
// the switch is the one moment where the two disagree by more than nothing —
// and reporting the loop's list there would hand the caller a run that appears
// to have done nothing at all.
func TestFailAfterHandoffFilterReportsUnfilteredLog(t *testing.T) {
	boom := errors.New("target agent refused to start")
	target := &agents.Agent{
		Name:      "Target",
		ModelImpl: agentstest.NewResponseBuilder().Text("never reached").Build(),
		OnStart: func(context.Context, *agents.RunContext) error {
			return boom
		},
	}

	handoff := agents.HandoffTo(target)
	// Drop the whole conversation on the way over: the strongest form of the
	// reset, and the one that leaves the loop's view empty.
	handoff.InputFilter = func(agents.HandoffInputData) agents.HandoffInputData {
		return agents.HandoffInputData{}
	}

	source := &agents.Agent{
		Name: "Source",
		ModelImpl: agentstest.NewResponseBuilder().
			FunctionCall(handoff.ToolName, "call_1", `{}`).
			Build(),
		Handoffs: []agents.Handoff{handoff},
	}

	_, err := agents.RunSync(t.Context(), source, "hand me over", agents.RunOptions{})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the target's OnStart error", err)
	}
	runErr, ok := errors.AsType[*agents.RunError](err)
	if !ok {
		t.Fatalf("err = %T, want *agents.RunError", err)
	}
	if runErr.Result == nil {
		t.Fatal("RunError.Result = nil, want the run's progress")
	}
	// The handoff call and its output both happened before the filter ran, so
	// both belong to the report.
	want := []string{"handoff_call", "handoff_output"}
	if got := agentstest.ItemTypes(runErr.Result.NewItems); !slices.Equal(got, want) {
		t.Errorf("NewItems = %v, want the pre-handoff log %v", got, want)
	}
	if runErr.Result.LastAgent != target {
		t.Errorf("LastAgent = %v, want the target agent", runErr.Result.LastAgent)
	}
}
