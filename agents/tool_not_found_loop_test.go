package agents_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/internal/agentstest"
)

func toolNotFoundOptions(limit int) agents.RunOptions {
	return agents.RunOptions{Exec: agents.ExecOptions{
		MaxTurns:             20,
		ToolNotFoundBehavior: agents.ToolNotFoundReturnToModel,
		ToolLoop:             agents.ToolLoopPolicy{MaxConsecutiveErrorTurns: limit},
	}}
}

// A model that keeps calling a tool that does not exist is looping like one
// calling a broken tool: the not-found output is a failed call, and the
// consecutive-error valve stops the run (spec §2.7d).
func TestUnknownToolTurnsTripTheLoopValve(t *testing.T) {
	model := agentstest.NewResponseBuilder().
		FunctionCall("ghost", "c1", `{}`).NewTurn().
		FunctionCall("ghost", "c2", `{}`).NewTurn().
		FunctionCall("ghost", "c3", `{}`).NewTurn().
		FunctionCall("ghost", "c4", `{}`).NewTurn().
		Text("never reached").
		Build()
	agent := &agents.Agent{Name: "a", ModelImpl: model}

	_, err := agents.RunSync(t.Context(), agent, "go", toolNotFoundOptions(3))
	tle, ok := errors.AsType[*agents.ToolLoopError](err)
	if !ok {
		t.Fatalf("err = %v, want *agents.ToolLoopError", err)
	}
	if tle.Turns != 3 {
		t.Errorf("turns = %d, want 3", tle.Turns)
	}
	agentstest.AssertModelCalls(t, model, 3)
}

// One successful call in between clears the counter, and the not-found output
// lands in call order beside it, marked as an error.
func TestUnknownToolCounterClearsOnASuccess(t *testing.T) {
	known := agents.NewTool("known", "", func(context.Context, *agents.ToolContext, struct{}) (string, error) {
		return "ok", nil
	})
	model := agentstest.NewResponseBuilder().
		FunctionCall("ghost", "c1", `{}`).NewTurn().
		FunctionCall("ghost", "c2", `{}`).NewTurn().
		FunctionCall("ghost", "c3", `{}`).FunctionCall("known", "c4", `{}`).NewTurn().
		FunctionCall("ghost", "c5", `{}`).NewTurn().
		FunctionCall("ghost", "c6", `{}`).NewTurn().
		Text("done").
		Build()
	agent := &agents.Agent{Name: "a", Tools: []*agents.Tool{known}, ModelImpl: model}

	res, err := agents.RunSync(t.Context(), agent, "go", toolNotFoundOptions(3))
	if err != nil {
		t.Fatalf("a success between the unknown calls tripped the valve: %v", err)
	}
	agentstest.AssertFinalOutput(t, res, "done")
	agentstest.AssertModelCalls(t, model, 6)

	var outputs []string
	for _, it := range res.NewItems {
		if it.Kind != agents.ItemToolCallOutput {
			continue
		}
		text, _ := it.Output.(string)
		outputs = append(outputs, text)
		if strings.Contains(text, "not found") != it.IsError {
			t.Errorf("output %q: IsError = %v, want it set on the not-found output only", text, it.IsError)
		}
	}
	want := []string{"Tool 'ghost' not found.", "Tool 'ghost' not found.", "Tool 'ghost' not found.", "ok", "Tool 'ghost' not found.", "Tool 'ghost' not found."}
	if got := strings.Join(outputs, "|"); got != strings.Join(want, "|") {
		t.Errorf("tool outputs in order = %q, want %q", got, strings.Join(want, "|"))
	}
}
