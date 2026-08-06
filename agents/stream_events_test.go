package agents_test

import (
	"testing"

	"github.com/zzir/agents-go/agents"
)

// TestStreamVocabularyWireNames spells the Responses stream vocabulary a
// second time, by hand, and holds the constants to it.
//
// Everything that used to spell these names now reads agents.Event*: the
// runner's classifiers, the modelkit event constructors, the OpenAI adapter's
// terminal-event switch, and conformancetest's closed set. That is the point
// of the constants — but it also means the synthesized events and the closed
// set that checks them can no longer disagree. A wrong VALUE propagates to
// producer and checker together and the whole conformance matrix stays green.
//
// This table is the one place that does not read the constants. It is the
// independent restatement of the wire protocol that the closed set used to be.
// A constant added to stream_events.go without a row here is an unpinned name.
func TestStreamVocabularyWireNames(t *testing.T) {
	for _, c := range []struct {
		got, want string
	}{
		{agents.EventResponseCreated, "response.created"},
		{agents.EventResponseInProgress, "response.in_progress"},
		{agents.EventResponseQueued, "response.queued"},

		{agents.EventResponseOutputItemAdded, "response.output_item.added"},
		{agents.EventResponseOutputItemDone, "response.output_item.done"},

		{agents.EventResponseContentPartAdded, "response.content_part.added"},
		{agents.EventResponseContentPartDone, "response.content_part.done"},

		{agents.EventResponseOutputTextDelta, "response.output_text.delta"},
		{agents.EventResponseOutputTextDone, "response.output_text.done"},

		{agents.EventResponseReasoningTextDelta, "response.reasoning_text.delta"},
		{agents.EventResponseReasoningTextDone, "response.reasoning_text.done"},

		{agents.EventResponseReasoningSummaryPartAdded, "response.reasoning_summary_part.added"},
		{agents.EventResponseReasoningSummaryPartDone, "response.reasoning_summary_part.done"},
		{agents.EventResponseReasoningSummaryTextDelta, "response.reasoning_summary_text.delta"},
		{agents.EventResponseReasoningSummaryTextDone, "response.reasoning_summary_text.done"},

		{agents.EventResponseFunctionCallArgumentsDelta, "response.function_call_arguments.delta"},
		{agents.EventResponseFunctionCallArgumentsDone, "response.function_call_arguments.done"},

		{agents.EventResponseCompleted, "response.completed"},
		{agents.EventResponseIncomplete, "response.incomplete"},

		{agents.EventError, "error"},
		{agents.EventResponseError, "response.error"},
		{agents.EventResponseFailed, "response.failed"},
	} {
		if c.got != c.want {
			t.Errorf("event constant = %q, want %q", c.got, c.want)
		}
	}
}
