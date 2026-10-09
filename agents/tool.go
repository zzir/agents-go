package agents

import (
	"context"
	"errors"
	"time"
)

// Tool is a tool the agent can call: the name and JSON-schema parameters shown
// to the model, plus the Go function run when the model calls it. The only
// tool type; every capability is a field — see spec §2.7c. Build one with
// NewTool, or directly for a hand-written schema (NewRawTool, MCP, a sandbox).
type Tool struct {
	// Name is the tool name exposed to the model.
	Name string
	// Description explains to the model what the tool does.
	Description string
	// ParamsJSONSchema is the JSON Schema for the tool's arguments object.
	ParamsJSONSchema map[string]any
	// Strict reports whether ParamsJSONSchema is strict-shaped and turns on
	// API-side strict validation. It describes the schema; NonStrict regenerates both.
	Strict bool
	// OnInvoke runs the tool on the raw JSON arguments the model emitted; nil
	// is a configuration error reported when called. TextResult covers the common case.
	OnInvoke func(ctx context.Context, tc *ToolContext, argsJSON string) (ToolResult, error)

	// ReadOnly is the tool's own hint that it only observes, read by plan-mode
	// gates; nothing enforces it.
	ReadOnly bool

	// IsEnabled, when non-nil, hides the tool from the model for a run on false.
	// Capture it before overwriting to compose — see spec §2.7c.
	IsEnabled func(ctx context.Context, rc *RunContext, agent *Agent) (bool, error)

	// Guardrails inspect this tool's calls (tool stages only). Append, never
	// assign, on a tool you did not build — see spec §2.7c.
	Guardrails []Guardrail

	// Timeout bounds one invocation; the call fails with a ToolTimeoutError
	// (fed back via FailureErrorFunction when set). Zero means no timeout.
	Timeout time.Duration

	// NeedsApproval pauses the run before this tool executes, surfacing a
	// ToolApprovalItem in RunResult.Interruptions.
	NeedsApproval bool
	// NeedsApprovalFunc decides per call and takes precedence over NeedsApproval;
	// callID tells concurrent calls to the same tool apart.
	NeedsApprovalFunc func(ctx context.Context, rc *RunContext, argsJSON string, callID string) (bool, error)

	// FailureErrorFunction turns an OnInvoke error into the tool output sent to
	// the model; nil aborts the run. NewTool installs DefaultToolErrorFunction.
	FailureErrorFunction func(ctx context.Context, tc *ToolContext, err error) string

	// Sequential runs the tool alone, in order, never concurrently with other tools.
	Sequential bool

	// Deferred withholds the tool from the model until a ToolResult names it in
	// AddedTools — see spec §2.7i.
	Deferred bool

	// RetrySafe declares the tool safe to run again after a crash interrupted
	// it (RetrySafeNames, session.RecoveryPolicy). Default unsafe.
	RetrySafe bool

	// validator is the compiled ParamsJSONSchema; nil on a hand-built literal.
	validator *schemaValidator

	// regen rebuilds schema and validator for a strictness; set by NewTool.
	regen func(strict bool) (map[string]any, *schemaValidator)
}

// NonStrict relaxes the tool's schema (",omitempty" fields stop being required)
// and returns the tool for chaining; call it before first use. On a tool not
// built by NewTool it only clears Strict.
func (t *Tool) NonStrict() *Tool {
	if t.regen != nil {
		t.ParamsJSONSchema, t.validator = t.regen(false)
	}
	t.Strict = false
	return t
}

// needsApproval resolves one call's approval: NeedsApprovalFunc when set, else
// NeedsApproval.
func (t *Tool) needsApproval(ctx context.Context, rc *RunContext, argsJSON, callID string) (bool, error) {
	if t.NeedsApprovalFunc != nil {
		return t.NeedsApprovalFunc(ctx, rc, argsJSON, callID)
	}
	return t.NeedsApproval, nil
}

// enabled reports whether the model should be shown this tool for the run.
func (t *Tool) enabled(ctx context.Context, rc *RunContext, agent *Agent) (bool, error) {
	if t.IsEnabled == nil {
		return true, nil
	}
	return t.IsEnabled(ctx, rc, agent)
}

// RetrySafeNames returns a session.RecoveryPolicy.RetrySafe predicate from the
// tools' RetrySafe flags.
func RetrySafeNames(tools []*Tool) func(string) bool {
	safe := map[string]bool{}
	for _, t := range tools {
		if t != nil && t.RetrySafe {
			safe[t.Name] = true
		}
	}
	return func(name string) bool { return safe[name] }
}

// DefaultToolErrorFunction is the default FailureErrorFunction: a model-readable
// message, with dedicated wording when the arguments were not valid JSON.
func DefaultToolErrorFunction(_ context.Context, _ *ToolContext, err error) string {
	if ae, ok := errors.AsType[*toolArgumentsJSONError](err); ok {
		return "An error occurred while parsing tool arguments. Please try again with valid JSON. Error: " + ae.cause.Error()
	}
	return "An error occurred while running the tool. Please try again. Error: " + err.Error()
}
