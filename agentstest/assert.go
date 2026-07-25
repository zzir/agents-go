package agentstest

import (
	"testing"

	"github.com/zzir/agents-go/agents"
)

// AssertFinalOutput fails the test unless the run's final output is the
// expected string.
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

// AssertScriptExhausted fails the test if scripted turns went unused — usually
// a sign the run stopped earlier than the test intended.
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
func ItemTypes(items []agents.RunItem) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.ItemType())
	}
	return out
}

// ToolCallNames returns the name of every tool the model asked to call, in
// order. Handoff calls are excluded — they surface as handoff items.
func ToolCallNames(items []agents.RunItem) []string {
	var out []string
	for _, it := range items {
		if tc, ok := it.(*agents.ToolCallItem); ok {
			out = append(out, tc.FunctionCall().Name)
		}
	}
	return out
}

// MessageTexts returns the text of every assistant message item, in order.
func MessageTexts(items []agents.RunItem) []string {
	var out []string
	for _, it := range items {
		if m, ok := it.(*agents.MessageOutputItem); ok {
			out = append(out, m.Text())
		}
	}
	return out
}

// CollectEvents drains a streamed run and returns its events. It fails the test
// on a stream error, so the caller can assert on the events directly.
func CollectEvents(tb testing.TB, sr *agents.StreamedResult) []agents.StreamEvent {
	tb.Helper()
	var events []agents.StreamEvent
	for ev, err := range sr.Events() {
		if err != nil {
			tb.Fatalf("stream error: %v", err)
		}
		events = append(events, ev)
	}
	return events
}

// RunItemEventNames returns the name of every run-item event in a stream, in
// order — the sequence a UI would render.
func RunItemEventNames(events []agents.StreamEvent) []string {
	var out []string
	for _, ev := range events {
		if e, ok := ev.(*agents.RunItemStreamEvent); ok {
			out = append(out, e.Name)
		}
	}
	return out
}
