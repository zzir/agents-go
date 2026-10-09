package agentstest

import (
	"testing"

	"github.com/zzir/agents-go/agents"
)

// AssertFinalOutput fails the test unless the run's final output is want.
func AssertFinalOutput(tb testing.TB, res *agents.RunResult, want string) {
	tb.Helper()
	if res == nil {
		tb.Fatalf("final output = <nil result>, want %q", want)
	}
	if got := res.FinalOutputString(); got != want {
		tb.Errorf("final output = %q, want %q", got, want)
	}
}

// AssertModelCalls fails the test unless the model was called exactly n times.
func AssertModelCalls(tb testing.TB, m *FakeModel, n int) {
	tb.Helper()
	if got := m.Calls(); got != n {
		tb.Errorf("model calls = %d, want %d", got, n)
	}
}

// AssertScriptExhausted fails the test if scripted turns went unused.
func AssertScriptExhausted(tb testing.TB, m *FakeModel) {
	tb.Helper()
	if n := m.Remaining(); n != 0 {
		tb.Errorf("%d scripted turn(s) unused; the run ended earlier than expected", n)
	}
}

// ItemTypes returns each item's discriminator, for order-sensitive assertions:
//
//	want := []string{"reasoning", "tool_call", "tool_call_output", "message_output"}
//	if got := agentstest.ItemTypes(res.NewItems); !slices.Equal(got, want) { ... }
func ItemTypes(items []*agents.RunItem) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, string(it.Kind))
	}
	return out
}

// ToolCallNames returns the name of every tool call item, in order; handoff
// calls surface as handoff items and are excluded.
func ToolCallNames(items []*agents.RunItem) []string {
	var out []string
	for _, it := range items {
		if it.Kind == agents.ItemToolCall {
			out = append(out, it.FunctionCall().Name)
		}
	}
	return out
}

// MessageTexts returns the text of every assistant message item, in order.
func MessageTexts(items []*agents.RunItem) []string {
	var out []string
	for _, it := range items {
		if it.Kind == agents.ItemMessage {
			out = append(out, it.Text())
		}
	}
	return out
}

// CollectEvents drives a run stream to completion and returns its events minus
// the terminal completion; a stream error fails the test.
func CollectEvents(tb testing.TB, stream agents.RunStream) []agents.StreamEvent {
	tb.Helper()
	events, _ := CollectRun(tb, stream)
	return events
}

// CollectRun drives a run stream to completion and returns its events and
// result; a stream error or a missing result fails the test.
func CollectRun(tb testing.TB, stream agents.RunStream) ([]agents.StreamEvent, *agents.RunResult) {
	tb.Helper()
	var events []agents.StreamEvent
	var res *agents.RunResult
	for ev, err := range stream {
		if err != nil {
			tb.Fatalf("stream error: %v", err)
		}
		if done, ok := ev.(*agents.RunCompletedEvent); ok {
			res = done.Result
			continue
		}
		events = append(events, ev)
	}
	if res == nil {
		tb.Fatal("the run stream ended without a result")
	}
	return events, res
}

// RunItemKinds returns the kind of every run-item event in a stream, in order.
func RunItemKinds(events []agents.StreamEvent) []agents.ItemKind {
	var out []agents.ItemKind
	for _, ev := range events {
		if e, ok := ev.(*agents.RunItemStreamEvent); ok {
			out = append(out, e.Item.Kind)
		}
	}
	return out
}
