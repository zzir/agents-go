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
	// Output is the guardrail's returned output (carrying its OutputInfo), so a
	// caller catching the tripwire can inspect what the guardrail reported —
	// Python parity with ToolGuardrail{Input,Output}GuardrailTripwireTriggered.
	Output ToolGuardrailFunctionOutput
}

// ToolInputGuardrailResult pairs a tool input guardrail with the output it
// produced for one tool call. Every executed guardrail is recorded — allow,
// reject and raise alike — so a caller reading RunResult.ToolInputGuardrailResults
// can inspect even non-tripping decisions (e.g. the guardrail's OutputInfo). It
// is the Go counterpart of the Python SDK's ToolInputGuardrailResult; ToolName
// and ToolCallID are Go-only conveniences identifying which call produced it.
type ToolInputGuardrailResult struct {
	Guardrail  ToolInputGuardrail
	ToolName   string
	ToolCallID string
	Output     ToolGuardrailFunctionOutput
}

// ToolOutputGuardrailResult pairs a tool output guardrail with the output it
// produced for one tool call (see ToolInputGuardrailResult). It is the Go
// counterpart of the Python SDK's ToolOutputGuardrailResult.
type ToolOutputGuardrailResult struct {
	Guardrail  ToolOutputGuardrail
	ToolName   string
	ToolCallID string
	Output     ToolGuardrailFunctionOutput
}
