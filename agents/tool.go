package agents

import (
	"context"
	"errors"
	"time"
)

// Tool is the sealed interface implemented by every tool the SDK supports. Tools
// are provider-agnostic: a Tool is a FunctionTool whose Go function the SDK
// executes locally, and the same Tool works against any model backend. The SDK
// deliberately does not model provider-hosted tools (e.g. OpenAI's server-side
// web_search), which would couple a tool to one backend.
//
// The unexported marker method keeps the set of tool kinds closed to this
// package; construct tools via the provided constructors (e.g. NewFunctionTool).
type Tool interface {
	// ToolName returns the tool's name as exposed to the model.
	ToolName() string
	isTool()
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
	// Strict reports whether ParamsJSONSchema is the strict-shaped schema
	// (every field required, unknown properties forbidden) and toggles OpenAI
	// strict-mode validation on the API side. It DESCRIBES the schema; setting
	// it after construction re-derives nothing — the advertised schema and the
	// local argument validator keep their built shape. To relax a
	// NewFunctionTool-built tool use NonStrict, which regenerates both.
	Strict bool
	// OnInvoke runs the tool. argsJSON is the raw JSON arguments string emitted
	// by the model.
	//
	// The result carries everything about the call, not just the model-facing
	// value: UI data that must not reach the model, the renderer to use, token
	// usage the tool spent itself, and whether the run should stop. Use
	// TextResult for the common case.
	OnInvoke func(ctx context.Context, tc *ToolContext, argsJSON string) (ToolResult, error)
	// IsEnabled, when non-nil, is consulted before exposing the tool to the
	// model; returning false hides the tool for that run.
	IsEnabled func(ctx context.Context, rc *RunContext, agent *Agent) (bool, error)

	// Guardrails inspect this tool's calls. Only their tool stages
	// (StageToolInput / StageToolOutput) are consulted; put run-wide guardrails
	// on the Agent instead.
	Guardrails []Guardrail

	// Timeout bounds a single invocation of this tool. When it expires the
	// invocation's context is canceled and the call fails with a
	// ToolTimeoutError (fed back to the model via FailureErrorFunction when
	// set, fatal otherwise). Zero means no timeout.
	Timeout time.Duration

	// NeedsApproval, when true, pauses the run before this tool executes,
	// surfacing a ToolApprovalItem in RunResult.Interruptions for a human to
	// approve or reject. Use NeedsApprovalFunc for per-call decisions.
	NeedsApproval bool
	// NeedsApprovalFunc, when non-nil, decides per call whether approval is
	// required, taking precedence over NeedsApproval. callID is the
	// model-assigned identifier of the specific tool call, so the predicate can
	// distinguish concurrent calls to the same tool. It mirrors the Python SDK's
	// needs_approval(run_context, tool_parameters, call_id).
	NeedsApprovalFunc func(ctx context.Context, rc *RunContext, argsJSON string, callID string) (bool, error)

	// FailureErrorFunction controls what happens when OnInvoke returns an error.
	// When non-nil, its returned message is sent back to the model as the tool
	// output (so the model can recover), matching the Python SDK's default. When
	// nil, the error aborts the run. NewFunctionTool installs
	// DefaultToolErrorFunction; set this field to nil to make tool errors fatal.
	FailureErrorFunction func(ctx context.Context, tc *ToolContext, err error) string

	// validator is the compiled form of ParamsJSONSchema, used to validate
	// model-sent arguments before they are decoded. Constructors set it
	// together with ParamsJSONSchema so the two cannot drift; a hand-built
	// literal leaves it nil and validates in its own OnInvoke.
	validator *schemaValidator

	// regen rebuilds the schema and validator for a given strictness. Only
	// NewFunctionTool installs it — the closure carries the argument type,
	// which is how NonStrict can re-reflect without a type parameter.
	regen func(strict bool) (map[string]any, *schemaValidator)
}

// NonStrict relaxes the tool's schema: fields whose json tag carries
// ",omitempty" stop being required, in both the schema advertised to the model
// and the local validation of incoming arguments. It returns the tool for
// chaining:
//
//	t := agents.NewFunctionTool("get_weather", "look up weather", weatherFn).NonStrict()
//
// Configure it before the tool is first used in a run. On a tool that was not
// built by NewFunctionTool it only clears Strict; the schema stays the
// caller's.
func (t *FunctionTool) NonStrict() *FunctionTool {
	if t.regen != nil {
		t.ParamsJSONSchema, t.validator = t.regen(false)
	}
	t.Strict = false
	return t
}

// DefaultToolErrorFunction is the default FailureErrorFunction: it returns a
// generic, model-readable error message. It mirrors the Python SDK's
// default_tool_error_function, including the dedicated wording when the model
// sent arguments that were not decodable JSON — the message carries only the
// underlying syntax error, prompting the model to retry with valid JSON.
func DefaultToolErrorFunction(_ context.Context, _ *ToolContext, err error) string {
	if ae, ok := errors.AsType[*toolArgumentsJSONError](err); ok {
		return "An error occurred while parsing tool arguments. Please try again with valid JSON. Error: " + ae.cause.Error()
	}
	return "An error occurred while running the tool. Please try again. Error: " + err.Error()
}

// ToolName implements Tool.
func (t *FunctionTool) ToolName() string { return t.Name }

func (t *FunctionTool) isTool() {}

var _ Tool = (*FunctionTool)(nil)

// FunctionTool declares its capabilities through the side interfaces, so a
// runner asks every tool the same way — ToolAs — whether the capability came
// from a field here or from a decorator wrapped around it.

// Invoke implements InvokableTool.
func (t *FunctionTool) Invoke(ctx context.Context, tc *ToolContext, argsJSON string) (ToolResult, error) {
	if t.OnInvoke == nil {
		return ToolResult{}, NewUserError("function tool %q has no OnInvoke", t.Name)
	}
	return t.OnInvoke(ctx, tc, argsJSON)
}

// ToolDescription implements DescribableTool.
func (t *FunctionTool) ToolDescription() string { return t.Description }

// ToolParamsSchema implements DescribableTool.
func (t *FunctionTool) ToolParamsSchema() map[string]any { return t.ParamsJSONSchema }

// ToolStrict implements DescribableTool.
func (t *FunctionTool) ToolStrict() bool { return t.Strict }

// NeedsToolApproval implements ApprovalRequiredTool.
func (t *FunctionTool) NeedsToolApproval(ctx context.Context, rc *RunContext, argsJSON, callID string) (bool, error) {
	if t.NeedsApprovalFunc != nil {
		return t.NeedsApprovalFunc(ctx, rc, argsJSON, callID)
	}
	return t.NeedsApproval, nil
}

// ToolGuardrails implements GuardedTool.
func (t *FunctionTool) ToolGuardrails() []Guardrail { return t.Guardrails }

// ToolTimeout implements TimeoutTool.
func (t *FunctionTool) ToolTimeout() time.Duration { return t.Timeout }

// IsToolEnabled implements EnableableTool.
func (t *FunctionTool) IsToolEnabled(ctx context.Context, rc *RunContext, agent *Agent) (bool, error) {
	if t.IsEnabled == nil {
		return true, nil
	}
	return t.IsEnabled(ctx, rc, agent)
}

// HandleToolFailure implements FailureHandlingTool. A tool whose
// FailureErrorFunction is nil deliberately does NOT satisfy the interface at
// runtime — the runner checks for nil — so its errors abort the run.
func (t *FunctionTool) HandleToolFailure(ctx context.Context, tc *ToolContext, err error) string {
	if t.FailureErrorFunction == nil {
		return ""
	}
	return t.FailureErrorFunction(ctx, tc, err)
}

var (
	_ InvokableTool        = (*FunctionTool)(nil)
	_ DescribableTool      = (*FunctionTool)(nil)
	_ ApprovalRequiredTool = (*FunctionTool)(nil)
	_ GuardedTool          = (*FunctionTool)(nil)
	_ TimeoutTool          = (*FunctionTool)(nil)
	_ EnableableTool       = (*FunctionTool)(nil)
	_ FailureHandlingTool  = (*FunctionTool)(nil)
)
