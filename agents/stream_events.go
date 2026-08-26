package agents

// The Responses stream vocabulary — the event names the SDK is written
// against (decisions §5.10).
//
// These live here, in the runner's own package, because that is the only home
// every user can reach: modelkit (the adapter toolkit that synthesizes these
// events) imports agents, so agents cannot import modelkit back. The runner's
// classifiers below, the modelkit event constructors, the OpenAI adapter's
// terminal-event switch and conformancetest's closed set all build from this
// one list, so an event name exists in exactly one place and a misspelled
// REFERENCE is a compile error rather than a branch that silently never fires.
//
// A misspelled VALUE is neither, and not a conformance failure either: the
// synthesized events and the closed set that checks them read the same
// constant, so they agree on a wrong name as readily as a right one. That is
// what stream_events_test.go is for — it pins every name below to its wire
// string, spelled independently. Adding a constant here means adding a row
// there, or the name arrives unpinned.
//
// The list is the whole vocabulary, not only the names this repo switches on:
// it is exported for adapter authors outside it, who need to know which events
// the runner accepts and the golden matrix allows. A name earns a place here
// by belonging to the Responses stream contract, not by having a caller here.
//
// The names are the wire strings and the constants are untyped, so they
// compare against ResponseStreamEvent.Type (a plain string on the Responses
// union) without conversion.
const (
	// EventResponseCreated opens every stream — the "a response is now in
	// flight" signal, and the first event an adapter must emit.
	EventResponseCreated = "response.created"
	// EventResponseInProgress reports that generation has begun. Like
	// created it carries no model output.
	EventResponseInProgress = "response.in_progress"
	// EventResponseQueued reports a response waiting for capacity before
	// generation starts. It is lifecycle preamble like created/in_progress
	// and the runner tolerates it wherever those appear, but ONLY a
	// pass-through backend emits it: a synthesized stream (modelkit) has no
	// queue to report. That is why conformancetest's closed set deliberately
	// leaves it out — an adapter emitting it would be reporting a queue it
	// does not have.
	EventResponseQueued = "response.queued"

	// EventResponseOutputItemAdded announces that an output item has begun.
	EventResponseOutputItemAdded = "response.output_item.added"
	// EventResponseOutputItemDone carries a finished output item. The
	// runner's stream accumulator collects exactly these as the fallback for
	// a terminal event whose output array is empty, so an adapter emits one
	// per item, in order.
	EventResponseOutputItemDone = "response.output_item.done"

	EventResponseContentPartAdded = "response.content_part.added"
	EventResponseContentPartDone  = "response.content_part.done"

	// EventResponseOutputTextDelta streams assistant text — the event the
	// agents-server UI renders visible output from.
	EventResponseOutputTextDelta = "response.output_text.delta"
	EventResponseOutputTextDone  = "response.output_text.done"

	// EventResponseReasoningTextDelta streams raw reasoning text, matching
	// where a reasoning item puts its final text (content, not summary).
	EventResponseReasoningTextDelta = "response.reasoning_text.delta"
	EventResponseReasoningTextDone  = "response.reasoning_text.done"

	EventResponseReasoningSummaryPartAdded = "response.reasoning_summary_part.added"
	EventResponseReasoningSummaryPartDone  = "response.reasoning_summary_part.done"
	EventResponseReasoningSummaryTextDelta = "response.reasoning_summary_text.delta"
	EventResponseReasoningSummaryTextDone  = "response.reasoning_summary_text.done"

	EventResponseFunctionCallArgumentsDelta = "response.function_call_arguments.delta"
	EventResponseFunctionCallArgumentsDone  = "response.function_call_arguments.done"

	// EventResponseCompleted is the successful terminal event; the runner
	// assembles its final ModelResponse from it, so its output list and usage
	// are what the run records — deltas are presentation only.
	EventResponseCompleted = "response.completed"
	// EventResponseIncomplete is the terminal event for a response that
	// arrived cut off. Reason max_output_tokens is the one recoverable
	// truncation (§2.7e); every other reason fails the call.
	EventResponseIncomplete = "response.incomplete"

	// EventError and EventResponseError are the two spellings the Responses
	// API uses for an error delivered as an ordinary stream event — one that
	// never trips the SSE layer's own error channel.
	EventError         = "error"
	EventResponseError = "response.error"
	// EventResponseFailed is the terminal event for a response the backend
	// gave up on.
	EventResponseFailed = "response.failed"
)

// streamLifecycleEvent reports whether a stream event type is pre-output
// lifecycle preamble: the response exists but nothing has been generated yet.
func streamLifecycleEvent(t string) bool {
	switch t {
	case EventResponseCreated, EventResponseInProgress, EventResponseQueued:
		return true
	}
	return false
}

// streamFailureEvent reports whether a stream event type announces a terminal
// failure. Like the lifecycle preamble it carries nothing the model generated,
// so it must not commit an attempt: the whole point of retry/fallback is to
// replace an attempt that ends in one of these. (response.incomplete is NOT
// here: a length-truncated response is output that arrived — committing on it
// is what stops a retry from throwing that output away.)
func streamFailureEvent(t string) bool {
	switch t {
	case EventError, EventResponseError, EventResponseFailed:
		return true
	}
	return false
}
