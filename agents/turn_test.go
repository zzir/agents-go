package agents

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// A turn hook sees what the turn was resolved to, not just what it produced.
func TestTurnSnapshot_DescribesTheResolvedTurn(t *testing.T) {
	tool := NewTool("probe", "probes", func(context.Context, *ToolContext, struct{}) (string, error) {
		return "ok", nil
	})
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "probe", "c1", `{}`)),
		modelResp(messageOutput(t, "done")),
	}}
	agent := &Agent{
		Name:         "a",
		Instructions: StaticInstructions("be brief"),
		Tools:        []*Tool{tool},
		ModelImpl:    model,
	}

	var snap *TurnSnapshot
	if _, err := RunSync(context.Background(), agent, "go", RunOptions{
		Exec: ExecOptions{ShouldStopAfterTurn: func(_ context.Context, tr *TurnResult) (bool, error) {
			snap = tr.Snapshot
			return false, nil
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if snap == nil {
		t.Fatal("the turn hook got no snapshot")
	}
	if snap.Agent != agent {
		t.Error("snapshot names the wrong agent")
	}
	if snap.Instructions != "be brief" {
		t.Errorf("instructions = %q, want the resolved system prompt", snap.Instructions)
	}
	if len(snap.Tools) != 1 || snap.Tools[0].Name != "probe" {
		t.Errorf("tools = %v, want the enabled ones", snap.Tools)
	}
	if snap.Model == nil {
		t.Error("snapshot has no resolved model")
	}
	if len(snap.Input) == 0 {
		t.Error("snapshot has no input")
	}
}

// PrepareNextTurn changes the next turn without mutating the Agent, which a
// concurrent run may be reading.
func TestPrepareNextTurn_ReshapesTheNextTurn(t *testing.T) {
	tool := NewTool("probe", "probes", func(context.Context, *ToolContext, struct{}) (string, error) {
		return "ok", nil
	})
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "probe", "c1", `{}`)),
		modelResp(messageOutput(t, "done")),
	}}
	agent := &Agent{Name: "a", Tools: []*Tool{tool}, ModelImpl: model}

	if _, err := RunSync(context.Background(), agent, "go", RunOptions{
		Exec: ExecOptions{PrepareNextTurn: func(_ context.Context, tr *TurnResult) (*TurnSnapshot, error) {
			// Withdraw the tool now that it has been used.
			next := *tr.Snapshot
			next.Tools = nil
			next.Instructions = "answer from what you already have"
			return &next, nil
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if len(model.lastReq.Tools) != 0 {
		t.Errorf("the second call offered %d tools, want none", len(model.lastReq.Tools))
	}
	if model.lastReq.SystemInstructions != "answer from what you already have" {
		t.Errorf("instructions = %q, want the prepared ones", model.lastReq.SystemInstructions)
	}
	// The agent itself is untouched.
	if len(agent.Tools) != 1 {
		t.Error("PrepareNextTurn mutated the agent")
	}
}

// The snapshot is used for one turn only: without a hook, each turn resolves
// afresh, so a dynamic instruction still changes between turns.
func TestPrepareNextTurn_AppliesToOneTurnOnly(t *testing.T) {
	calls := 0
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "probe", "c1", `{}`)),
		modelResp(functionCallOutput(t, "probe", "c2", `{}`)),
		modelResp(messageOutput(t, "done")),
	}}
	tool := NewTool("probe", "probes", func(context.Context, *ToolContext, struct{}) (string, error) {
		return "ok", nil
	})
	agent := &Agent{Name: "a", Tools: []*Tool{tool}, ModelImpl: model,
		Instructions: InstructionsFunc(func(context.Context, *RunContext, *Agent) (string, error) {
			calls++
			return "resolved", nil
		})}

	prepared := 0
	if _, err := RunSync(context.Background(), agent, "go", RunOptions{
		Exec: ExecOptions{PrepareNextTurn: func(_ context.Context, tr *TurnResult) (*TurnSnapshot, error) {
			prepared++
			if prepared == 1 {
				next := *tr.Snapshot
				next.Instructions = "one turn only"
				return &next, nil
			}
			return nil, nil
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if model.lastReq.SystemInstructions != "resolved" {
		t.Errorf("instructions = %q, want the agent's own on the turn after the prepared one",
			model.lastReq.SystemInstructions)
	}
}

func TestPrepareNextTurn_ErrorFailsTheRun(t *testing.T) {
	tool := NewTool("probe", "probes", func(context.Context, *ToolContext, struct{}) (string, error) {
		return "ok", nil
	})
	model := &fakeModel{responses: []*ModelResponse{modelResp(functionCallOutput(t, "probe", "c1", `{}`))}}
	agent := &Agent{Name: "a", Tools: []*Tool{tool}, ModelImpl: model}

	_, err := RunSync(context.Background(), agent, "go", RunOptions{
		Exec: ExecOptions{PrepareNextTurn: func(context.Context, *TurnResult) (*TurnSnapshot, error) {
			return nil, errors.New("no snapshot for you")
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "no snapshot for you") {
		t.Fatalf("err = %v, want the hook's error", err)
	}
}

func TestTurnResult_ToolCallNames(t *testing.T) {
	tr := &TurnResult{NewItems: []*RunItem{
		NewModelItem(ItemToolCall, nil, functionCallOutput(t, "alpha", "c1", `{}`)),
		NewModelItem(ItemMessage, nil, messageOutput(t, "hi")),
		NewModelItem(ItemToolCall, nil, functionCallOutput(t, "beta", "c2", `{}`)),
	}}
	got := tr.ToolCallNames()
	if len(got) != 2 || got[0] != "alpha" || got[1] != "beta" {
		t.Errorf("ToolCallNames() = %v, want [alpha beta] in call order", got)
	}
}

// A prepared snapshot is nearly always a copy of the previous turn's, so
// honoring its Input would replay that turn — the tool call and its output
// silently missing from what the model is sent next.
func TestPrepareNextTurn_CannotReplayTheLastTurnsInput(t *testing.T) {
	tool := NewTool("probe", "probes", func(context.Context, *ToolContext, struct{}) (string, error) {
		return "TOOL-RESULT", nil
	})
	model := &fakeModel{responses: []*ModelResponse{
		modelResp(functionCallOutput(t, "probe", "c1", `{}`)),
		modelResp(messageOutput(t, "done")),
	}}
	agent := &Agent{Name: "a", Tools: []*Tool{tool}, ModelImpl: model}

	if _, err := RunSync(context.Background(), agent, "go", RunOptions{
		Exec: ExecOptions{PrepareNextTurn: func(_ context.Context, tr *TurnResult) (*TurnSnapshot, error) {
			next := *tr.Snapshot // carries the FIRST turn's input
			return &next, nil
		}},
	}); err != nil {
		t.Fatal(err)
	}
	// user input + the call + its output.
	if got := len(model.lastReq.Input); got < 3 {
		t.Errorf("second call sent %d items, want the turn's real input (the tool call and output are missing)", got)
	}
}
