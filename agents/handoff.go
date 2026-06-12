package agents

import (
	"context"
	"regexp"
	"strings"
)

// HandoffInputData is passed to a Handoff's InputFilter to transform the
// conversation the next agent receives. InputHistory is the full conversation as
// model input items, up to and including the handoff. A common use is to trim
// earlier tool calls before delegating.
type HandoffInputData struct {
	InputHistory []TResponseInputItem
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
	InputJSONSchema map[string]any
	// StrictJSONSchema toggles strict-mode validation of the handoff input.
	StrictJSONSchema bool
	// AgentName is the name of the target agent, used for tracing.
	AgentName string

	// OnInvoke is called by the runner when the handoff is selected; it returns
	// the agent to switch to.
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

var invalidToolNameChars = regexp.MustCompile(`[^a-zA-Z0-9_]`)

// transformToolName sanitizes a name for function calling: spaces and other
// invalid characters become underscores, and the result is lowercased. It
// mirrors transform_string_function_style in the Python SDK.
func transformToolName(name string) string {
	return strings.ToLower(invalidToolNameChars.ReplaceAllString(strings.ReplaceAll(name, " ", "_"), "_"))
}

// HandoffTo builds a Handoff that delegates the run to target. The resulting
// tool is named "transfer_to_<target>" (sanitized) and takes no input. It is
// the Go counterpart of Python's handoff(agent).
//
// To customize the tool name/description or require input, construct a Handoff
// struct directly.
func HandoffTo(target *Agent) Handoff {
	desc := "Handoff to the " + target.Name + " agent to handle the request."
	if target.HandoffDescription != "" {
		desc += " " + target.HandoffDescription
	}
	return Handoff{
		ToolName:         transformToolName("transfer_to_" + target.Name),
		ToolDescription:  desc,
		InputJSONSchema:  emptyStrictSchema(),
		StrictJSONSchema: true,
		AgentName:        target.Name,
		OnInvoke: func(context.Context, *RunContext, string) (*Agent, error) {
			return target, nil
		},
	}
}
