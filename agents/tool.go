package agents

import "context"

// Tool is the sealed interface implemented by every tool kind the SDK supports
// (function tools, hosted tools such as web search, etc). It mirrors the Python
// SDK's Tool union type.
//
// The unexported marker method keeps the set of tool kinds closed to this
// package; construct tools via the provided constructors (e.g. NewFunctionTool).
type Tool interface {
	// ToolName returns the tool's name as exposed to the model.
	ToolName() string
	isTool()
}

// FunctionToolResult is the outcome of invoking a function tool.
type FunctionToolResult struct {
	// ToolName is the name of the tool that produced this result.
	ToolName string
	// Output is the value returned by the tool function. It is serialized to a
	// string when sent back to the model.
	Output any
}

// FunctionTool is a tool backed by a Go function. The model is shown the tool's
// name, description and JSON-schema parameters; when the model calls the tool,
// OnInvoke is run with the raw JSON arguments.
//
// Construct function tools with NewFunctionTool, which derives ParamsJSONSchema
// from the argument type via reflection. The struct is exported so advanced
// callers can build one by hand.
type FunctionTool struct {
	// Name is the tool name exposed to the model.
	Name string
	// Description explains to the model what the tool does.
	Description string
	// ParamsJSONSchema is the JSON Schema for the tool's arguments object.
	ParamsJSONSchema map[string]any
	// Strict toggles OpenAI strict-mode schema validation.
	Strict bool
	// OnInvoke runs the tool. argsJSON is the raw JSON arguments string emitted
	// by the model. The returned value is serialized back to the model.
	OnInvoke func(ctx context.Context, tc *ToolContext, argsJSON string) (any, error)
	// IsEnabled, when non-nil, is consulted before exposing the tool to the
	// model; returning false hides the tool for that run.
	IsEnabled func(ctx context.Context, rc *RunContext, agent *Agent) (bool, error)

	// InputGuardrails inspect the tool arguments before OnInvoke runs.
	InputGuardrails []ToolInputGuardrail
	// OutputGuardrails inspect the tool's result after OnInvoke runs.
	OutputGuardrails []ToolOutputGuardrail

	// NeedsApproval, when true, pauses the run before this tool executes,
	// surfacing a ToolApprovalItem in RunResult.Interruptions for a human to
	// approve or reject. Use NeedsApprovalFunc for per-call decisions.
	NeedsApproval bool
	// NeedsApprovalFunc, when non-nil, decides per call whether approval is
	// required, taking precedence over NeedsApproval.
	NeedsApprovalFunc func(ctx context.Context, rc *RunContext, argsJSON string) (bool, error)

	// FailureErrorFunction controls what happens when OnInvoke returns an error.
	// When non-nil, its returned message is sent back to the model as the tool
	// output (so the model can recover), matching the Python SDK's default. When
	// nil, the error aborts the run. NewFunctionTool installs
	// DefaultToolErrorFunction; set this field to nil to make tool errors fatal.
	FailureErrorFunction func(ctx context.Context, tc *ToolContext, err error) string
}

// DefaultToolErrorFunction is the default FailureErrorFunction: it returns a
// generic, model-readable error message. It mirrors the Python SDK's
// default_tool_error_function.
func DefaultToolErrorFunction(_ context.Context, _ *ToolContext, err error) string {
	return "An error occurred while running the tool. Please try again. Error: " + err.Error()
}

// requiresApproval reports whether a specific call to this tool needs approval.
func (t *FunctionTool) requiresApproval(ctx context.Context, rc *RunContext, argsJSON string) (bool, error) {
	if t.NeedsApprovalFunc != nil {
		return t.NeedsApprovalFunc(ctx, rc, argsJSON)
	}
	return t.NeedsApproval, nil
}

// ToolName implements Tool.
func (t *FunctionTool) ToolName() string { return t.Name }

func (t *FunctionTool) isTool() {}

var _ Tool = (*FunctionTool)(nil)
