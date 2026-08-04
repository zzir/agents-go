package agents

import (
	"context"
	"fmt"
)

// multipleHandoffsMessage is sent back as the tool output for every handoff
// beyond the first when the model requests several in one turn. It matches the
// Python SDK's message.
const multipleHandoffsMessage = "Multiple handoffs detected, ignoring this one."

// executeHandoff switches to the first requested handoff target, recording a
// synthetic handoff output item.
func (r *runner) executeHandoff(ctx context.Context, from *Agent, handoffs []toolRunHandoff, newStepItems []RunItem) (*singleStepResult, error) {
	run := handoffs[0]
	// Every handoff call the model emitted is in the conversation as a
	// function_call; the ones we ignore still need an output item, or the next
	// model call is rejected for a dangling call.
	for _, ignored := range handoffs[1:] {
		newStepItems = append(newStepItems, newFunctionCallOutputItem(from, ignored.Call.CallID, multipleHandoffsMessage))
	}
	span := r.trace.StartHandoffSpan(run.Handoff.ToolName, r.agentParentID())
	defer span.Finish()
	// Validate the handoff arguments against the handoff's input schema before it
	// fires, so a handoff that expects input but receives none (or invalid input)
	// is rejected as a *ModelBehaviorError instead of silently transferring with
	// zero-valued input (Python parity: handoffs/__init__.py:278-307).
	if verr := validateHandoffInput(&run.Handoff, run.Call.Arguments); verr != nil {
		span.SetError(verr.Error(), map[string]any{"details": "invalid handoff input"})
		return nil, verr
	}
	if run.Handoff.OnInvoke == nil {
		return nil, newUserError("handoff %q has no OnInvoke", run.Handoff.ToolName)
	}
	target, err := run.Handoff.OnInvoke(ctx, r.rc, run.Call.Arguments)
	if err != nil {
		return nil, fmt.Errorf("handoff %q failed: %w", run.Handoff.ToolName, err)
	}
	if target == nil {
		return nil, newModelBehaviorError("handoff %q returned a nil agent", run.Handoff.ToolName)
	}
	if run.Handoff.OnHandoff != nil {
		if err := run.Handoff.OnHandoff(ctx, r.rc, run.Call.Arguments); err != nil {
			return nil, fmt.Errorf("handoff %q on-handoff callback failed: %w", run.Handoff.ToolName, err)
		}
	}

	outputItem := &HandoffOutputItem{
		Agent:       from,
		Raw:         handoffOutputInput(run.Call.CallID, target.Name),
		SourceAgent: from,
		TargetAgent: target,
	}
	newStepItems = append(newStepItems, outputItem)

	h := run.Handoff
	return &singleStepResult{
		NewStepItems: newStepItems,
		NextStep:     stepHandoff,
		NewAgent:     target,
		Handoff:      &h,
	}, nil
}

// applyHandoffInputFilter builds the full conversation input and runs filter
// over it, returning the filtered input for the next agent.
func applyHandoffInputFilter(filter func(HandoffInputData) HandoffInputData, originalInput []TResponseInputItem, generated []RunItem) ([]TResponseInputItem, error) {
	full, err := buildModelInput(originalInput, generated)
	if err != nil {
		return nil, err
	}
	out := filter(HandoffInputData{InputHistory: full})
	return out.InputHistory, nil
}

// handoffInputFilter resolves the filter for a handoff: the handoff's own
// InputFilter takes precedence over the run-level RunOptions.HandoffInputFilter.
// Returns nil when neither is set.
func (r *runner) handoffInputFilter(h *Handoff) func(HandoffInputData) HandoffInputData {
	if h.InputFilter != nil {
		return h.InputFilter
	}
	return r.opts.Exec.HandoffInputFilter
}

func lastMessageItem(items []RunItem) *MessageOutputItem {
	for i := len(items) - 1; i >= 0; i-- {
		if m, ok := items[i].(*MessageOutputItem); ok {
			return m
		}
	}
	return nil
}

// toolOwnGuardrails returns whatever guardrails a tool declares, from a field
// or from a decorator — the runner does not need to know which.
