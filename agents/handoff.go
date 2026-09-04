package agents

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
)

// HandoffInputData is passed to a Handoff's InputFilter to transform the
// conversation the next agent receives; InputHistory is the full conversation
// as model input items, up to and including the handoff.
type HandoffInputData struct {
	InputHistory []InputItem
}

// Handoff represents the ability for one agent to delegate a run to another
// agent. It is surfaced to the model as a tool; when the model "calls" it, the
// runner switches the active agent.
type Handoff struct {
	// ToolName is the name exposed to the model (e.g. "transfer_to_billing").
	ToolName string
	// ToolDescription explains when to hand off.
	ToolDescription string
	// InputJSONSchema is the JSON Schema for the (optional) handoff input. The
	// model's arguments are validated against the whole schema before the
	// handoff fires, and a violation is a *ModelBehaviorError (spec §2.7h);
	// nil skips validation. Unless NonStrictSchema is set the schema is sent
	// as strict-mode, so a hand-built one must already be in the strict subset.
	InputJSONSchema map[string]any
	// NonStrictSchema opts the handoff input out of strict-mode schema
	// validation, for schemas strict mode cannot express. The zero value is
	// strict — which is why the field is spelled as an opt-out.
	NonStrictSchema bool
	// AgentName is the name of the target agent, used for tracing.
	AgentName string

	// Target is the agent this handoff switches to, declared as data so a
	// consumer can enumerate the handoff graph without invoking user code.
	// HandoffTo fills it; a dynamic handoff sets OnInvoke instead and leaves
	// Target nil.
	Target *Agent

	// OnInvoke, when non-nil, resolves the handoff target at runtime — it may
	// pick an agent from the arguments — and takes precedence over Target.
	// When nil, the runner uses Target; a Handoff with neither fails the run
	// with a *UserError when the model selects it.
	OnInvoke func(ctx context.Context, rc *RunContext, argsJSON string) (*Agent, error)

	// OnHandoff, when non-nil, is invoked when the handoff fires, before control
	// passes to the target agent. argsJSON is the raw handoff input. Use it for
	// side effects such as logging or fetching data for the next agent.
	OnHandoff func(ctx context.Context, rc *RunContext, argsJSON string) error

	// InputFilter, when non-nil, transforms the conversation the target agent
	// receives (e.g. removing prior tool calls). It does not affect what is saved
	// to the session.
	InputFilter func(HandoffInputData) HandoffInputData

	// IsEnabled, when non-nil, gates whether this handoff is offered to the model.
	IsEnabled func(ctx context.Context, rc *RunContext, agent *Agent) (bool, error)
}

// validateHandoffInput checks the raw arguments against InputJSONSchema before
// the handoff fires: whole schema, no defaults applied, compiled per call (spec §2.7h).
func validateHandoffInput(h *Handoff, argsJSON string) error {
	if len(h.InputJSONSchema) == 0 {
		// No schema is nothing to check against, not a stricter default: such a
		// handoff's OnInvoke reads the raw argument string however it likes.
		return nil
	}
	trimmed := strings.TrimSpace(argsJSON)
	// "" and "null" spell "no input"; whether the handoff accepts it is read
	// off the required list directly, so it holds for an uncompilable schema.
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
	// A non-object instance is rejected explicitly: required/properties say
	// nothing about a bare scalar (spec §2.7h).
	if _, ok := parsed.(map[string]any); !ok {
		return NewModelBehaviorError("invalid input for handoff %q: expected a JSON object", h.ToolName)
	}
	if err := newSchemaValidator(h.InputJSONSchema).Validate([]byte(trimmed)); err != nil {
		return NewModelBehaviorError("invalid input for handoff %q: %v", h.ToolName, err)
	}
	return nil
}

var invalidToolNameChars = regexp.MustCompile(`[^a-zA-Z0-9_]`)

// transformToolName sanitizes a name for function calling: spaces and other
// invalid characters become underscores, and the result is lowercased.
func transformToolName(name string) string {
	return strings.ToLower(invalidToolNameChars.ReplaceAllString(strings.ReplaceAll(name, " ", "_"), "_"))
}

// HandoffTo builds a Handoff that delegates the run to target: a tool named
// "transfer_to_<target>" (sanitized) taking no input, with Target declared
// statically so the handoff is plain data. To customize the name or require
// input, construct a Handoff directly.
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
