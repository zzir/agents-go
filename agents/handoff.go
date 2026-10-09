package agents

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
)

// HandoffInputData is what a Handoff's InputFilter transforms: the full
// conversation as model input items, up to and including the handoff.
type HandoffInputData struct {
	InputHistory []InputItem
}

// Handoff lets one agent delegate the run to another. It is surfaced to the
// model as a tool; when the model calls it, the runner switches the active agent.
type Handoff struct {
	// ToolName is the name exposed to the model (e.g. "transfer_to_billing").
	ToolName string
	// ToolDescription explains when to hand off.
	ToolDescription string
	// InputJSONSchema validates the model's arguments before the handoff fires
	// (a violation is a *ModelBehaviorError); nil skips validation — see spec §2.7h.
	InputJSONSchema map[string]any
	// NonStrictSchema opts the handoff input out of strict mode; the zero value
	// is strict.
	NonStrictSchema bool
	// AgentName is the name of the target agent, used for tracing.
	AgentName string

	// Target is the agent this handoff switches to, as data; HandoffTo fills
	// it, a dynamic handoff sets OnInvoke instead and leaves it nil.
	Target *Agent

	// OnInvoke resolves the target at runtime and takes precedence over Target;
	// a Handoff with neither fails the run with a *UserError when selected.
	OnInvoke func(ctx context.Context, rc *RunContext, argsJSON string) (*Agent, error)

	// OnHandoff runs when the handoff fires, before control passes to the
	// target; argsJSON is the raw handoff input.
	OnHandoff func(ctx context.Context, rc *RunContext, argsJSON string) error

	// InputFilter transforms the conversation the target agent receives; it
	// does not affect what is saved to the session.
	InputFilter func(HandoffInputData) HandoffInputData

	// IsEnabled, when non-nil, gates whether this handoff is offered to the model.
	IsEnabled func(ctx context.Context, rc *RunContext, agent *Agent) (bool, error)
}

// validateHandoffInput checks the raw arguments against InputJSONSchema before
// the handoff fires: whole schema, no defaults, compiled per call — see spec §2.7h.
func validateHandoffInput(h *Handoff, argsJSON string) error {
	if len(h.InputJSONSchema) == 0 {
		return nil
	}
	trimmed := strings.TrimSpace(argsJSON)
	// "" and "null" spell "no input"; the required list decides, so an
	// uncompilable schema still answers.
	if trimmed == "" || trimmed == "null" {
		if required, _ := h.InputJSONSchema["required"].([]any); len(required) > 0 {
			return NewModelBehaviorError("Handoff function expected non-null input, but got None")
		}
		trimmed = "{}"
	}
	var parsed any
	if err := json.Unmarshal([]byte(trimmed), &parsed); err != nil {
		return NewModelBehaviorError("invalid input for handoff %q: invalid JSON: %v", h.ToolName, err)
	}
	// A bare scalar is rejected explicitly: required/properties say nothing about it.
	if _, ok := parsed.(map[string]any); !ok {
		return NewModelBehaviorError("invalid input for handoff %q: expected a JSON object", h.ToolName)
	}
	if err := newSchemaValidator(h.InputJSONSchema).Validate([]byte(trimmed)); err != nil {
		return NewModelBehaviorError("invalid input for handoff %q: %v", h.ToolName, err)
	}
	return nil
}

var invalidToolNameChars = regexp.MustCompile(`[^a-zA-Z0-9_]`)

// transformToolName sanitizes a name for function calling: invalid characters
// become underscores, lowercased.
func transformToolName(name string) string {
	return strings.ToLower(invalidToolNameChars.ReplaceAllString(strings.ReplaceAll(name, " ", "_"), "_"))
}

// HandoffTo builds a no-input Handoff named "transfer_to_<target>" with Target
// set; construct a Handoff directly to customize the name or require input.
func HandoffTo(target *Agent) Handoff {
	desc := "Handoff to the " + target.Name + " agent to handle the request."
	if target.HandoffDescription != "" {
		desc += " " + target.HandoffDescription
	}
	return Handoff{
		ToolName:        transformToolName("transfer_to_" + target.Name),
		ToolDescription: desc,
		InputJSONSchema: emptyStrictSchema(),
		AgentName:       target.Name,
		Target:          target,
	}
}
