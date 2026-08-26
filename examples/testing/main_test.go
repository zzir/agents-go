package main

import (
	"context"
	"testing"

	"github.com/zzir/agents-go/agents"
)

func TestAgentCallsToolThenAnswers(t *testing.T) {
	model := callThenAnswer()

	res, err := agents.RunSync(context.Background(), newAgent(), "weather in Beijing?",
		agents.RunOptions{Model: agents.ModelOptions{Override: model}})
	if err != nil {
		t.Fatal(err)
	}

	if got, want := res.FinalOutputString(), "It is sunny and 21°C in Beijing."; got != want {
		t.Errorf("final output = %q, want %q", got, want)
	}

	// A run that stopped earlier than the test assumed is exactly what a test
	// should catch, so assert the script was fully consumed.
	if model.calls != len(model.responses) {
		t.Errorf("consumed %d of %d scripted turns", model.calls, len(model.responses))
	}

	// The tool really ran: its output is in the run's items.
	var sawToolOutput bool
	for _, it := range res.NewItems {
		if it.Kind == agents.ItemToolCallOutput {
			sawToolOutput = true
		}
	}
	if !sawToolOutput {
		t.Error("no tool output item — the tool did not execute")
	}
}

func TestScriptExhaustionIsAnError(t *testing.T) {
	// One response, but the script's function call needs a second turn.
	model := &scriptedModel{responses: []*agents.ModelResponse{
		{Output: []agents.OutputItem{functionCall("get_weather", "call_1", `{"city":"Beijing"}`)}},
	}}

	if _, err := agents.RunSync(context.Background(), newAgent(), "weather?",
		agents.RunOptions{Model: agents.ModelOptions{Override: model}}); err == nil {
		t.Fatal("expected the exhausted script to fail the run")
	}
}
