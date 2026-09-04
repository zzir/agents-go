package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
)

// RunErrorData is a snapshot of the run's progress passed to a
// RunErrorHandler.
type RunErrorData struct {
	// Input is the run's original input (after session history was prepended
	// and any handoff input filter was applied).
	Input []InputItem
	// NewItems are the items generated so far. It is nil when the run failed
	// before generating anything.
	NewItems []*RunItem
	// History is Input followed by Output: the full conversation as the next
	// model call would have seen it.
	History []InputItem
	// Output is NewItems converted to model-input form.
	Output []InputItem
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
	// records the message.
	ExcludeFromHistory bool
}

// RunErrorHandler recovers a failing run by supplying a fallback final output.
// Return (nil, nil) to decline recovery — the original error is returned
// unchanged. Returning a non-nil error aborts the run with that error instead.
type RunErrorHandler func(ctx context.Context, in RunErrorHandlerInput) (*RunErrorHandlerResult, error)

// RunErrorHandlers holds per-error-kind recovery handlers. Each handler turns
// its error into a normal run completion with a fallback final output; nil
// handlers leave that error fatal.
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
	// text and this handler is nil (or declines), the runner calls the model
	// again instead of failing.
	InvalidFinalOutput RunErrorHandler
}

// buildRunErrorData snapshots the run for a handler; items that cannot convert
// are skipped, never raised — this path is already failing.
func buildRunErrorData(input []InputItem, newItems []*RunItem, raw []*ModelResponse, lastAgent *Agent) RunErrorData {
	output := make([]InputItem, 0, len(newItems))
	for _, it := range newItems {
		in, err := it.ToInputItem()
		if err != nil {
			continue
		}
		output = append(output, in)
	}
	history := make([]InputItem, 0, len(input)+len(output))
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

// errorRecovery is a successful handler outcome: the validated fallback output
// and, unless the handler opted out, the synthesized assistant message.
type errorRecovery struct {
	finalOutput any
	message     *RunItem // nil when ExcludeFromHistory was set
}

// resolveErrorRecovery invokes handler for cause, validates its fallback
// against the output schema and synthesizes the message; (nil, nil) declines.
func (r *runner) resolveErrorRecovery(
	ctx context.Context,
	kind string,
	handler RunErrorHandler,
	cause error,
	agent *Agent,
	input []InputItem,
	newItems []*RunItem,
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
		msg, merr := synthesizeMessageOutputItem(agent, formatFinalOutputText(agent, validated), kind)
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

// marshalFinalOutputPayload renders a final output as the JSON the model would
// produce for the schema, adding the {"response": ...} envelope when used.
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

// validateHandlerFinalOutput validates a handler's fallback against the output
// schema; a bad fallback is a *UserError — the handler produced it.
func validateHandlerFinalOutput(agent *Agent, v any) (any, error) {
	schema := agentOutputSchema(agent)
	if schema.IsPlainText() {
		return v, nil
	}
	payload, err := marshalFinalOutputPayload(schema, v)
	if err != nil {
		return nil, NewUserError("invalid run error handler FinalOutput for structured output: %v", err)
	}
	validated, err := schema.ValidateJSON(payload)
	if err != nil {
		return nil, NewUserError("invalid run error handler FinalOutput for structured output: %v", err)
	}
	return validated, nil
}

// formatFinalOutputText renders a final output as the synthesized message's
// text: the value for plain text, its JSON payload for structured output.
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
// handler's fallback text, via JSON so it matches a model-produced one; no id.
func synthesizeMessageOutputItem(agent *Agent, text, handlerKind string) (*RunItem, error) {
	payload := map[string]any{
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
	var item OutputItem
	if err := json.Unmarshal(raw, &item); err != nil {
		return nil, err
	}
	it := NewModelItem(ItemMessage, agent, item)
	it.Source = Source{Type: SourceErrorHandler, ID: handlerKind}
	return it, nil
}
