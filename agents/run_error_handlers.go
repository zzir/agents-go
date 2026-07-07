package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
)

// fakeResponsesID marks output items synthesized by the SDK rather than
// returned by the model, matching the Python SDK's FAKE_RESPONSES_ID.
const fakeResponsesID = "__fake_id__"

// RunErrorData is a snapshot of the run's progress passed to a RunErrorHandler.
// It mirrors the Python SDK's RunErrorData.
type RunErrorData struct {
	// Input is the run's original input (after session history was prepended
	// and any handoff input filter was applied).
	Input []TResponseInputItem
	// NewItems are the items generated so far.
	NewItems []RunItem
	// History is Input followed by Output: the full conversation as the next
	// model call would have seen it.
	History []TResponseInputItem
	// Output is NewItems converted to model-input form.
	Output []TResponseInputItem
	// RawResponses are the raw model responses backing the error: every
	// response so far for a max-turns overrun, the current turn's response for
	// a refusal or invalid final output.
	RawResponses []*ModelResponse
	// LastAgent is the agent that was active when the error occurred.
	LastAgent *Agent
}

// RunErrorHandlerInput is the argument passed to a RunErrorHandler.
type RunErrorHandlerInput struct {
	// Error is the failure being handled: a *MaxTurnsError, *ModelRefusalError
	// or *ModelBehaviorError depending on which handler is invoked.
	Error error
	// RunContext is the run's context wrapper (user data and usage).
	RunContext *RunContext
	// RunData is a snapshot of the run's progress.
	RunData RunErrorData
}

// RunErrorHandlerResult is what a RunErrorHandler returns to recover the run.
type RunErrorHandlerResult struct {
	// FinalOutput becomes the run's final output. For an agent with a
	// structured output type it must marshal to JSON that validates against
	// the output schema; otherwise the run fails with a *UserError.
	FinalOutput any
	// ExcludeFromHistory, when true, skips synthesizing an assistant message
	// carrying FinalOutput into the run's items (and session). The zero value
	// records the message — matching Python's include_in_history=True default.
	ExcludeFromHistory bool
}

// RunErrorHandler recovers a failing run by supplying a fallback final output.
// Return (nil, nil) to decline recovery — the original error is returned
// unchanged. Returning a non-nil error aborts the run with that error instead.
type RunErrorHandler func(ctx context.Context, in RunErrorHandlerInput) (*RunErrorHandlerResult, error)

// RunErrorHandlers holds per-error-kind recovery handlers, the counterpart of
// Python's Runner.run(..., error_handlers={...}). Each handler turns its error
// into a normal run completion with a fallback final output; nil handlers
// leave that error fatal.
type RunErrorHandlers struct {
	// MaxTurns is consulted when the run exceeds its turn budget
	// (*MaxTurnsError).
	MaxTurns RunErrorHandler
	// ModelRefusal is consulted when the model refuses to respond
	// (*ModelRefusalError).
	ModelRefusal RunErrorHandler
	// InvalidFinalOutput is consulted when an agent with a structured output
	// type produces a final message that fails schema validation, or no final
	// text at all (*ModelBehaviorError). Other model-behavior errors (e.g.
	// calling an unknown tool) are not routed here. When the model produced no
	// text and this handler is nil (or declines), the runner keeps Python's
	// behavior of running the model again instead of failing.
	InvalidFinalOutput RunErrorHandler
}

// buildRunErrorData snapshots the run for a handler invocation. Items that
// cannot convert to input form are skipped from History/Output (mirroring
// Python's run_item_to_input_item returning None), never an error — this runs
// on a path that is already failing.
func buildRunErrorData(input []TResponseInputItem, newItems []RunItem, raw []*ModelResponse, lastAgent *Agent) RunErrorData {
	output := make([]TResponseInputItem, 0, len(newItems))
	for _, it := range newItems {
		in, err := it.ToInputItem()
		if err != nil {
			continue
		}
		output = append(output, in)
	}
	history := make([]TResponseInputItem, 0, len(input)+len(output))
	history = append(history, input...)
	history = append(history, output...)
	return RunErrorData{
		Input:        input,
		NewItems:     slices.Clone(newItems),
		History:      history,
		Output:       output,
		RawResponses: slices.Clone(raw),
		LastAgent:    lastAgent,
	}
}

// errorRecovery is a successful handler outcome: the validated fallback final
// output and, unless the handler opted out of history, the synthesized
// assistant message carrying it.
type errorRecovery struct {
	finalOutput any
	message     *MessageOutputItem // nil when ExcludeFromHistory was set
}

// resolveErrorRecovery invokes handler for cause and converts its result into
// an errorRecovery: the fallback output is validated against the agent's
// output schema and the optional history message is synthesized. A nil
// handler, or one that returns (nil, nil), yields (nil, nil) — the caller
// surfaces the original error (or keeps its default behavior).
func (r *runner) resolveErrorRecovery(
	ctx context.Context,
	handler RunErrorHandler,
	cause error,
	agent *Agent,
	input []TResponseInputItem,
	newItems []RunItem,
	raw []*ModelResponse,
) (*errorRecovery, error) {
	if handler == nil {
		return nil, nil
	}
	res, err := handler(ctx, RunErrorHandlerInput{
		Error:      cause,
		RunContext: r.rc,
		RunData:    buildRunErrorData(input, newItems, raw, agent),
	})
	if err != nil {
		return nil, err
	}
	if res == nil {
		return nil, nil
	}
	validated, err := validateHandlerFinalOutput(agent, res.FinalOutput)
	if err != nil {
		return nil, err
	}
	rec := &errorRecovery{finalOutput: validated}
	if !res.ExcludeFromHistory {
		msg, merr := synthesizeMessageOutputItem(agent, formatFinalOutputText(agent, validated))
		if merr != nil {
			return nil, merr
		}
		rec.message = msg
	}
	return rec, nil
}

// wrappedOutputSchema reports whether the schema wraps non-object outputs in a
// {"response": ...} envelope (see OutputType).
func wrappedOutputSchema(s OutputSchema) bool {
	if c, ok := s.(interface{ wrappedSchema() bool }); ok {
		return c.wrappedSchema()
	}
	return false
}

// marshalFinalOutputPayload renders a final output value as the JSON payload
// in the shape the model itself would produce for the schema — wrapping it in
// the {"response": ...} envelope when the schema uses one, unless the value
// already carries the envelope key.
func marshalFinalOutputPayload(schema OutputSchema, v any) (string, error) {
	payload := v
	if wrappedOutputSchema(schema) {
		wrap := true
		if m, ok := v.(map[string]any); ok {
			if _, has := m[wrapperDictKey]; has {
				wrap = false
			}
		}
		if wrap {
			payload = map[string]any{wrapperDictKey: v}
		}
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// validateHandlerFinalOutput validates a handler's fallback output against the
// agent's output schema, returning the validated value exactly as ValidateJSON
// would produce it for model output. A fallback that does not marshal or
// validate is a *UserError — the handler, not the model, produced it.
func validateHandlerFinalOutput(agent *Agent, v any) (any, error) {
	schema := agentOutputSchema(agent)
	if schema.IsPlainText() {
		return v, nil
	}
	payload, err := marshalFinalOutputPayload(schema, v)
	if err == nil {
		validated, verr := schema.ValidateJSON(payload)
		if verr == nil {
			return validated, nil
		}
		err = verr
	}
	return nil, newUserError("invalid run error handler FinalOutput for structured output: %v", err)
}

// formatFinalOutputText renders a final output as the text of the synthesized
// assistant message: the value itself for plain text, its JSON payload for
// structured outputs.
func formatFinalOutputText(agent *Agent, v any) string {
	schema := agentOutputSchema(agent)
	if !schema.IsPlainText() {
		if payload, err := marshalFinalOutputPayload(schema, v); err == nil {
			return payload
		}
	}
	return fmt.Sprint(v)
}

// synthesizeMessageOutputItem builds a completed assistant message carrying a
// handler's fallback output text, marked with the fake response id (the
// counterpart of Python's create_message_output_item). It round-trips through
// JSON so the union item is indistinguishable from a model-produced one.
func synthesizeMessageOutputItem(agent *Agent, text string) (*MessageOutputItem, error) {
	payload := map[string]any{
		"id":     fakeResponsesID,
		"type":   "message",
		"role":   "assistant",
		"status": "completed",
		"content": []map[string]any{{
			"type":        "output_text",
			"text":        text,
			"annotations": []any{},
		}},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	var item TResponseOutputItem
	if err := json.Unmarshal(raw, &item); err != nil {
		return nil, err
	}
	return &MessageOutputItem{Agent: agent, Raw: item}, nil
}
