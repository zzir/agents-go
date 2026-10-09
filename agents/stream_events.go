package agents

// The Responses stream event types, the whole vocabulary (decisions §5.10).
// Untyped, so they compare against ResponseStreamEvent.Type unconverted;
// stream_events_test.go pins each to its wire string.
const (
	// EventResponseCreated opens every stream; the first event an adapter must emit.
	EventResponseCreated = "response.created"
	// EventResponseInProgress reports that generation has begun; no output yet.
	EventResponseInProgress = "response.in_progress"
	// EventResponseQueued is lifecycle preamble only a pass-through backend
	// emits — see spec §2.15.
	EventResponseQueued = "response.queued"

	// EventResponseOutputItemAdded announces that an output item has begun.
	EventResponseOutputItemAdded = "response.output_item.added"
	// EventResponseOutputItemDone carries a finished output item, one per item
	// in order; the runner's fallback when the terminal event's output is empty.
	EventResponseOutputItemDone = "response.output_item.done"

	EventResponseContentPartAdded = "response.content_part.added"
	EventResponseContentPartDone  = "response.content_part.done"

	// EventResponseOutputTextDelta streams assistant text.
	EventResponseOutputTextDelta = "response.output_text.delta"
	EventResponseOutputTextDone  = "response.output_text.done"

	// EventResponseReasoningTextDelta streams raw reasoning text (content, not
	// summary).
	EventResponseReasoningTextDelta = "response.reasoning_text.delta"
	EventResponseReasoningTextDone  = "response.reasoning_text.done"

	EventResponseReasoningSummaryPartAdded = "response.reasoning_summary_part.added"
	EventResponseReasoningSummaryPartDone  = "response.reasoning_summary_part.done"
	EventResponseReasoningSummaryTextDelta = "response.reasoning_summary_text.delta"
	EventResponseReasoningSummaryTextDone  = "response.reasoning_summary_text.done"

	EventResponseFunctionCallArgumentsDelta = "response.function_call_arguments.delta"
	EventResponseFunctionCallArgumentsDone  = "response.function_call_arguments.done"

	// EventResponseCompleted is the successful terminal event; its output list
	// and usage are what the run records.
	EventResponseCompleted = "response.completed"
	// EventResponseIncomplete is the terminal event for a cut-off response;
	// only reason max_output_tokens is recoverable — see spec §2.7e.
	EventResponseIncomplete = "response.incomplete"

	// EventError and EventResponseError are the two spellings of an error
	// delivered as an ordinary stream event.
	EventError         = "error"
	EventResponseError = "response.error"
	// EventResponseFailed is the terminal event for a response the backend gave up on.
	EventResponseFailed = "response.failed"
)

// streamLifecycleEvent reports whether an event type is pre-output lifecycle preamble.
func streamLifecycleEvent(t string) bool {
	switch t {
	case EventResponseCreated, EventResponseInProgress, EventResponseQueued:
		return true
	}
	return false
}

// streamFailureEvent reports whether an event type announces terminal failure;
// response.incomplete is real output, not failure.
func streamFailureEvent(t string) bool {
	switch t {
	case EventError, EventResponseError, EventResponseFailed:
		return true
	}
	return false
}
