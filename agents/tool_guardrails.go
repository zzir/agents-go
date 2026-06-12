package agents

import "context"

// ToolGuardrailBehavior selects what happens when a tool guardrail fires.
type ToolGuardrailBehavior int

const (
	// ToolGuardrailAllow lets tool execution proceed normally (the default).
	ToolGuardrailAllow ToolGuardrailBehavior = iota
	// ToolGuardrailRejectContent rejects the tool call/output but continues the
	// run, substituting Message as the content sent to the model.
	ToolGuardrailRejectContent
	// ToolGuardrailRaiseException halts the run with a tripwire error.
	ToolGuardrailRaiseException
)

// ToolGuardrailFunctionOutput is what a tool guardrail returns. Build one with
// AllowTool, RejectToolContent or RaiseToolException.
type ToolGuardrailFunctionOutput struct {
	OutputInfo any
	Behavior   ToolGuardrailBehavior
	// Message is the replacement content when Behavior is RejectContent.
	Message string
}

// AllowTool allows normal tool execution.
func AllowTool(outputInfo any) ToolGuardrailFunctionOutput {
	return ToolGuardrailFunctionOutput{OutputInfo: outputInfo, Behavior: ToolGuardrailAllow}
}

// RejectToolContent rejects the call/output but continues, sending message to
// the model in place of the tool's content.
func RejectToolContent(message string, outputInfo any) ToolGuardrailFunctionOutput {
	return ToolGuardrailFunctionOutput{OutputInfo: outputInfo, Behavior: ToolGuardrailRejectContent, Message: message}
}

// RaiseToolException halts the run with a tripwire error.
func RaiseToolException(outputInfo any) ToolGuardrailFunctionOutput {
	return ToolGuardrailFunctionOutput{OutputInfo: outputInfo, Behavior: ToolGuardrailRaiseException}
}

// ToolInputGuardrailData is passed to a tool input guardrail.
type ToolInputGuardrailData struct {
	Agent      *Agent
	ToolName   string
	ToolCallID string
	Arguments  string
}

// ToolOutputGuardrailData is passed to a tool output guardrail.
type ToolOutputGuardrailData struct {
	Agent      *Agent
	ToolName   string
	ToolCallID string
	Arguments  string
	Output     any
}

// ToolInputGuardrail inspects a tool call's arguments before the tool runs.
type ToolInputGuardrail struct {
	Name string
	Run  func(ctx context.Context, rc *RunContext, data ToolInputGuardrailData) (ToolGuardrailFunctionOutput, error)
}

// ToolOutputGuardrail inspects a tool's output after it runs.
type ToolOutputGuardrail struct {
	Name string
	Run  func(ctx context.Context, rc *RunContext, data ToolOutputGuardrailData) (ToolGuardrailFunctionOutput, error)
}

// ToolGuardrailTripwireError is returned when a tool guardrail raises.
type ToolGuardrailTripwireError struct {
	AgentsError
	GuardrailName string
	ToolName      string
}
