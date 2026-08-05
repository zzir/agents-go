package agentstest_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agentstest"
)

type weatherArgs struct {
	City string `json:"city"`
}

func weatherTool(t *testing.T, seen *[]string) *agents.FunctionTool {
	t.Helper()
	return agents.NewFunctionTool("get_weather", "look up weather",
		func(_ context.Context, _ *agents.ToolContext, a weatherArgs) (string, error) {
			*seen = append(*seen, a.City)
			return "sunny in " + a.City, nil
		})
}

func TestTextModel_SingleTurn(t *testing.T) {
	model := agentstest.TextModel("hello world")
	agent := &agents.Agent{Name: "a", ModelImpl: model}

	res, err := agents.RunSync(context.Background(), agent, "hi", agents.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	agentstest.AssertFinalOutput(t, res, "hello world")
	agentstest.AssertModelCalls(t, model, 1)
	agentstest.AssertScriptExhausted(t, model)

	if got := model.LastRequest().SystemInstructions; got != "" {
		t.Errorf("system instructions = %q, want empty", got)
	}
}

func TestResponseBuilder_ToolCallThenAnswer(t *testing.T) {
	var seen []string
	model := agentstest.NewResponseBuilder().
		Reasoning("the user wants weather").
		FunctionCall("get_weather", "call_1", `{"city":"SF"}`).
		NewTurn().
		Text("it is sunny").
		Build()

	agent := &agents.Agent{
		Name:      "a",
		Tools:     []*agents.FunctionTool{weatherTool(t, &seen)},
		ModelImpl: model,
	}

	res, err := agents.RunSync(context.Background(), agent, "weather in SF?", agents.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	agentstest.AssertFinalOutput(t, res, "it is sunny")
	agentstest.AssertModelCalls(t, model, 2)
	agentstest.AssertScriptExhausted(t, model)

	if !slices.Equal(seen, []string{"SF"}) {
		t.Errorf("tool saw cities %v, want [SF]", seen)
	}
	if got := agentstest.ToolCallNames(res.NewItems); !slices.Equal(got, []string{"get_weather"}) {
		t.Errorf("tool calls = %v", got)
	}
	want := []string{"reasoning", "tool_call", "tool_call_output", "message_output"}
	if got := agentstest.ItemTypes(res.NewItems); !slices.Equal(got, want) {
		t.Errorf("item types = %v, want %v", got, want)
	}
	// The second turn must carry the call and its output back to the model.
	if n := len(model.LastRequest().Input); n < 3 {
		t.Errorf("second turn input has %d items, want at least 3", n)
	}
}

func TestUsageAccumulates(t *testing.T) {
	model := agentstest.NewResponseBuilder().
		FunctionCall("get_weather", "c1", `{"city":"SF"}`).
		Usage(agents.RequestUsage{InputTokens: 10, OutputTokens: 2, TotalTokens: 12}).
		NewTurn().
		Text("done").
		Usage(agents.RequestUsage{InputTokens: 20, OutputTokens: 5, TotalTokens: 25}).
		Build()

	var seen []string
	agent := &agents.Agent{Name: "a", Tools: []*agents.FunctionTool{weatherTool(t, &seen)}, ModelImpl: model}
	res, err := agents.RunSync(context.Background(), agent, "hi", agents.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Usage.TotalTokens != 37 {
		t.Errorf("total tokens = %d, want 37", res.Usage.TotalTokens)
	}
	if res.Usage.Requests != 2 {
		t.Errorf("requests = %d, want 2", res.Usage.Requests)
	}
	if n := len(res.Usage.RequestUsageEntries); n != 2 {
		t.Errorf("per-request entries = %d, want 2", n)
	}
}

func TestFail_SurfacesModelError(t *testing.T) {
	sentinel := errors.New("provider exploded")
	model := agentstest.NewResponseBuilder().Fail(sentinel).Build()
	agent := &agents.Agent{Name: "a", ModelImpl: model}

	_, err := agents.RunSync(context.Background(), agent, "hi", agents.RunOptions{})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want it to wrap %v", err, sentinel)
	}
}

func TestRefusal_FailsTheRun(t *testing.T) {
	model := agentstest.NewResponseBuilder().Refusal("I can't help with that").Build()
	agent := &agents.Agent{Name: "a", ModelImpl: model}

	_, err := agents.RunSync(context.Background(), agent, "hi", agents.RunOptions{})
	var refusal *agents.ModelRefusalError
	if !errors.As(err, &refusal) {
		t.Fatalf("err = %v (%T), want *agents.ModelRefusalError", err, err)
	}
	if refusal.Refusal != "I can't help with that" {
		t.Errorf("refusal = %q", refusal.Refusal)
	}
}

func TestExhaustedScript_YieldsEmptyOutput(t *testing.T) {
	// One scripted turn, but the tool call forces a second model call the
	// script does not cover: the runner sees an empty response and finishes.
	var seen []string
	model := agentstest.NewResponseBuilder().
		FunctionCall("get_weather", "c1", `{"city":"SF"}`).
		Build()
	agent := &agents.Agent{Name: "a", Tools: []*agents.FunctionTool{weatherTool(t, &seen)}, ModelImpl: model}

	res, err := agents.RunSync(context.Background(), agent, "hi", agents.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	agentstest.AssertFinalOutput(t, res, "")
	agentstest.AssertModelCalls(t, model, 2)
}

func TestStreaming_EventsAndFinalResult(t *testing.T) {
	model := agentstest.NewResponseBuilder().
		FunctionCall("get_weather", "c1", `{"city":"SF"}`).
		NewTurn().
		Text("it is sunny").
		Build()

	var seen []string
	agent := &agents.Agent{Name: "a", Tools: []*agents.FunctionTool{weatherTool(t, &seen)}, ModelImpl: model}

	stream, _ := agents.Run(context.Background(), agent, "hi", agents.RunOptions{})
	events, res := agentstest.CollectRun(t, stream)
	agentstest.AssertFinalOutput(t, res, "it is sunny")

	names := agentstest.RunItemEventNames(events)
	for _, want := range []string{"tool_called", "tool_output", "message_output_created"} {
		if !slices.Contains(names, want) {
			t.Errorf("stream missing %q event; got %v", want, names)
		}
	}
}

func TestStreamTextDeltas(t *testing.T) {
	model := agentstest.TextModel("hey")
	model.StreamTextDeltas = true
	agent := &agents.Agent{Name: "a", ModelImpl: model}

	stream, _ := agents.Run(context.Background(), agent, "hi", agents.RunOptions{})
	var delta strings.Builder
	for ev, err := range stream {
		if err != nil {
			t.Fatal(err)
		}
		raw, ok := ev.(*agents.RawResponsesStreamEvent)
		if !ok || raw.Data.Type != "response.output_text.delta" {
			continue
		}
		delta.WriteString(raw.Data.AsResponseOutputTextDelta().Delta)
	}
	if delta.String() != "hey" {
		t.Errorf("assembled deltas = %q, want %q", delta.String(), "hey")
	}
}

func TestRaw_UnknownItemTypeIsKept(t *testing.T) {
	// A type the SDK does not model. It used to be dropped; it is now kept as
	// an UnknownOutputItem and resent verbatim, because dropping it corrupts
	// the conversation rather than merely ignoring a feature.
	model := agentstest.NewResponseBuilder().
		Raw(`{"type":"some_future_call","id":"x1","status":"completed"}`).
		Text("done anyway").
		Build()
	agent := &agents.Agent{Name: "a", ModelImpl: model}

	res, err := agents.RunSync(context.Background(), agent, "hi", agents.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	agentstest.AssertFinalOutput(t, res, "done anyway")
	if got := agentstest.ItemTypes(res.NewItems); !slices.Equal(got, []string{"unknown", "message_output"}) {
		t.Errorf("item types = %v, want the unknown item kept alongside the message", got)
	}
}

func TestMessageTexts(t *testing.T) {
	model := agentstest.TextModel("first")
	agent := &agents.Agent{Name: "a", ModelImpl: model}
	res, err := agents.RunSync(context.Background(), agent, "hi", agents.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := agentstest.MessageTexts(res.NewItems); !slices.Equal(got, []string{"first"}) {
		t.Errorf("message texts = %v", got)
	}
}

func TestBuilder_TrailingEmptyTurnDropped(t *testing.T) {
	model := agentstest.NewResponseBuilder().Text("only").NewTurn().Build()
	if got := model.Remaining(); got != 1 {
		t.Errorf("scripted turns = %d, want 1 (trailing empty turn should be dropped)", got)
	}
}
