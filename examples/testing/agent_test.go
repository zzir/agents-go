package main

import (
	"context"
	"errors"
	"testing"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agentstest"
)

// The point of every test below: no API key, no network, no flakiness. The
// model is scripted, so the thing under test is your agent — its instructions,
// its tools, and what it does with a result — not the model's mood.

// The ordinary case: the model calls the tool, then answers from the result.
// Two turns, because a tool call and the answer that follows it are two model
// calls.
func TestAgentCallsWeatherToolThenAnswers(t *testing.T) {
	model := agentstest.NewResponseBuilder().
		FunctionCall("get_weather", "call_1", `{"city":"SF"}`).
		NewTurn().
		Text("It's sunny and 23°C in SF.").
		Build()

	res, err := agents.RunSync(context.Background(), newAgent(), "What's the weather in SF?",
		agents.RunOptions{Model: agents.ModelOptions{Override: model}})
	if err != nil {
		t.Fatal(err)
	}

	agentstest.AssertFinalOutput(t, res, "It's sunny and 23°C in SF.")
	agentstest.AssertModelCalls(t, model, 2)
	// The tool ran, and it ran once.
	if got := agentstest.ToolCallNames(res.NewItems); len(got) != 1 || got[0] != "get_weather" {
		t.Fatalf("tool calls = %v, want [get_weather]", got)
	}
	// Nothing scripted went unused — a script longer than the run usually
	// means the agent stopped earlier than the test assumed.
	agentstest.AssertScriptExhausted(t, model)
}

// What your tool returned has to actually reach the next model call. This is
// the assertion that catches an agent wired up so the result never gets back.
func TestToolResultReachesTheModel(t *testing.T) {
	model := agentstest.NewResponseBuilder().
		FunctionCall("get_weather", "call_1", `{"city":"Paris"}`).
		NewTurn().
		Text("done").
		Build()

	res, err := agents.RunSync(context.Background(), newAgent(), "weather in Paris?",
		agents.RunOptions{Model: agents.ModelOptions{Override: model}})
	if err != nil {
		t.Fatal(err)
	}

	var outputs []any
	for _, it := range res.NewItems {
		if out, ok := it.(*agents.ToolCallOutputItem); ok {
			outputs = append(outputs, out.Output)
		}
	}
	if len(outputs) != 1 || outputs[0] != "sunny, 23°C in Paris" {
		t.Fatalf("tool outputs = %v, want the Paris reading", outputs)
	}
}

// A model call can fail. Scripting the failure is how you test that your
// program surfaces it rather than answering with something invented.
func TestModelFailureSurfaces(t *testing.T) {
	boom := errors.New("upstream is down")
	model := agentstest.NewResponseBuilder().Fail(boom).Build()

	_, err := agents.RunSync(context.Background(), newAgent(), "weather?",
		agents.RunOptions{Model: agents.ModelOptions{Override: model}})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it to wrap %v", err, boom)
	}
}

// Streaming runs are testable the same way: CollectRun drives the stream and
// hands back both the events and the result, so a test can assert on the
// sequence a UI would render as well as on the answer.
func TestStreamingEmitsTheToolCallBeforeTheAnswer(t *testing.T) {
	model := agentstest.NewResponseBuilder().
		FunctionCall("get_weather", "call_1", `{"city":"SF"}`).
		NewTurn().
		Text("sunny").
		Build()

	stream, _ := agents.Run(context.Background(), newAgent(), "weather in SF?",
		agents.RunOptions{Model: agents.ModelOptions{Override: model}})
	events, res := agentstest.CollectRun(t, stream)

	agentstest.AssertFinalOutput(t, res, "sunny")
	names := agentstest.RunItemEventNames(events)
	if len(names) == 0 {
		t.Fatal("no run-item events in the stream")
	}
	// The call is announced before its output, and both before the answer.
	var order []string
	for _, n := range names {
		switch n {
		case "tool_called", "tool_output", "message_output_created":
			order = append(order, n)
		}
	}
	want := []string{"tool_called", "tool_output", "message_output_created"}
	if len(order) != len(want) {
		t.Fatalf("event order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("event order = %v, want %v", order, want)
		}
	}
}

// TextModel is the shorthand when the agent has nothing to call: one string
// per turn.
func TestPlainAnswerNeedsNoBuilder(t *testing.T) {
	model := agentstest.TextModel("42")

	res, err := agents.RunSync(context.Background(), newAgent(), "the answer?",
		agents.RunOptions{Model: agents.ModelOptions{Override: model}})
	if err != nil {
		t.Fatal(err)
	}
	agentstest.AssertFinalOutput(t, res, "42")
}
