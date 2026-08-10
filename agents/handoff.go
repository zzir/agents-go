package agents

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
)

// HandoffInputData is passed to a Handoff's InputFilter to transform the
// conversation the next agent receives. InputHistory is the full conversation as
// model input items, up to and including the handoff. A common use is to trim
// earlier tool calls before delegating.
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
	// InputJSONSchema is the JSON Schema for the (optional) handoff input.
	//
	// The model's arguments are validated against the WHOLE schema before the
	// handoff fires — nested required keys, types, enums and bounds included —
	// and a violation is a *ModelBehaviorError, so input the target agent could
	// not have used never reaches it. Arguments must be a JSON object; absent
	// ones ("" or "null") are read as "{}", which a schema declaring
	// root-level required keys rejects and one requiring nothing (the default
	// HandoffTo transfer) accepts.
	//
	// A nil schema skips validation entirely. A schema this SDK cannot compile
	// keeps the checks that need no compilation — arguments still have to be a
	// JSON object, and still have to be present when the schema declares
	// required keys — and skips the rest.
	//
	// The schema is sent to the provider as written: unless NonStrictSchema is
	// set it is sent as a strict-mode schema, so a hand-built one has to be in
	// the strict subset already (see EnsureStrictJSONSchema) or the API rejects
	// the request.
	InputJSONSchema map[string]any
	// NonStrictSchema opts the handoff input out of strict-mode schema
	// validation, for schemas strict mode cannot express. The zero value is
	// strict — which is why the field is spelled as an opt-out.
	NonStrictSchema bool
	// AgentName is the name of the target agent, used for tracing.
	AgentName string

	// Target is the agent this handoff switches to, declared as data so a
	// consumer can enumerate the handoff graph without invoking user code.
	// HandoffTo fills it; when OnInvoke is nil the runner switches to Target
	// directly, so a hand-built static handoff needs no callback.
	//
	// A dynamic handoff sets OnInvoke instead and leaves Target nil: nil is how
	// it says "not statically enumerable".
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

// validateHandoffInput checks the raw handoff arguments against the handoff's
// InputJSONSchema before the handoff fires, so input the target agent could not
// have used is a *ModelBehaviorError fed back to the model instead of a silent
// transfer with zero-valued input. The check is the whole schema, nested
// included, the same one tool arguments get.
//
// Arguments are checked as sent, without applying schema defaults: OnHandoff,
// OnInvoke and the session all see the model's raw argument string, and a value
// invented here would not be in it.
//
// The schema is compiled here rather than cached on the Handoff, which the
// runner copies per turn: one compilation per invocation is nothing next to the
// model call that produced the arguments.
func validateHandoffInput(h *Handoff, argsJSON string) error {
	if len(h.InputJSONSchema) == 0 {
		// No schema is nothing to check against, not a stricter default: such a
		// handoff's OnInvoke reads the raw argument string however it likes.
		return nil
	}
	trimmed := strings.TrimSpace(argsJSON)
	// "" and "null" are how a model spells "no input". Whether the handoff can
	// accept it is read off the schema's required list directly, so it still
	// holds for a schema we cannot compile.
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
	// A non-object instance is rejected explicitly: `required`/`properties` say
	// nothing about a bare scalar, so a schema omitting "type" would otherwise
	// accept one as the silent zero-value transfer this check prevents.
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

// HandoffTo builds a Handoff that delegates the run to target. The resulting
// tool is named "transfer_to_<target>" (sanitized) and takes no input. The
// target is declared statically (Target), not wrapped in an OnInvoke closure,
// so the built handoff is plain data a consumer can enumerate.
//
// To customize the tool name/description or require input, construct a Handoff
// struct directly.
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
