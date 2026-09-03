package agents

import (
	"context"
	"errors"
	"time"
)

// Tool is a tool the agent can call: a name and JSON-schema parameters shown
// to the model, plus the Go function the SDK runs when the model calls it.
// It is a struct, and the only kind of tool there is (decisions §5.4); every
// capability is a field, so deriving a variant is copying the struct
// (spec §2.7c). Build one with NewTool, or directly for a hand-written schema
// (NewRawTool, an MCP bridge, a sandbox).
type Tool struct {
	// Name is the tool name exposed to the model.
	Name string
	// Description explains to the model what the tool does.
	Description string
	// ParamsJSONSchema is the JSON Schema for the tool's arguments object.
	ParamsJSONSchema map[string]any
	// Strict reports whether ParamsJSONSchema is strict-shaped and turns on
	// API-side strict validation. It DESCRIBES the schema: setting it after
	// construction re-derives nothing; NonStrict regenerates both.
	Strict bool
	// OnInvoke runs the tool. argsJSON is the raw JSON arguments string the
	// model emitted. A tool with no OnInvoke is a configuration error, reported
	// when the model calls it. Use TextResult for the common case.
	OnInvoke func(ctx context.Context, tc *ToolContext, argsJSON string) (ToolResult, error)

	// ReadOnly marks a tool that only observes. A HINT the tool declares about
	// itself, read by gates that admit observation while withholding change
	// (plan mode); nothing enforces it.
	ReadOnly bool

	// IsEnabled, when non-nil, is consulted before exposing the tool to the
	// model; returning false hides it for that run. To gate a tool you did not
	// build, capture the field before overwriting it and call it from yours.
	IsEnabled func(ctx context.Context, rc *RunContext, agent *Agent) (bool, error)

	// Guardrails inspect this tool's calls; only their tool stages are
	// consulted (run-wide guardrails go on the Agent). Append to a tool you did
	// not build, never assign — replacing the slice disarms its own checks.
	Guardrails []Guardrail

	// Timeout bounds one invocation: the context is canceled and the call
	// fails with a ToolTimeoutError (fed back via FailureErrorFunction when
	// set, fatal otherwise). Zero means no timeout.
	Timeout time.Duration

	// NeedsApproval, when true, pauses the run before this tool executes,
	// surfacing a ToolApprovalItem in RunResult.Interruptions for a human to
	// approve or reject. Use NeedsApprovalFunc for per-call decisions.
	NeedsApproval bool
	// NeedsApprovalFunc, when non-nil, decides per call, taking precedence over
	// NeedsApproval. callID is the model-assigned id of the call, so the
	// predicate can tell concurrent calls to the same tool apart.
	NeedsApprovalFunc func(ctx context.Context, rc *RunContext, argsJSON string, callID string) (bool, error)

	// FailureErrorFunction turns an OnInvoke error into the tool output sent
	// back to the model; nil makes the error abort the run. NewTool installs
	// DefaultToolErrorFunction.
	FailureErrorFunction func(ctx context.Context, tc *ToolContext, err error) string

	// Sequential marks a tool that must not run concurrently with other tools
	// in the same turn. The turn runs it alone, in order.
	Sequential bool

	// Deferred withholds the tool from the model until a ToolResult names it
	// in AddedTools; once disclosed it stays available for the rest of the run
	// (spec §2.7i).
	Deferred bool

	// RetrySafe declares the tool safe to run again after a crash interrupted
	// it mid-execution (see RetrySafeNames, session.RecoveryPolicy). Default
	// unsafe: a reader qualifies; anything that writes or charges does not.
	RetrySafe bool

	// validator is the compiled ParamsJSONSchema, set together with it by the
	// constructors; a hand-built literal leaves it nil and validates itself.
	validator *schemaValidator

	// regen rebuilds schema and validator for a strictness; NewTool installs it
	// carrying the argument type, which is how NonStrict re-reflects.
	regen func(strict bool) (map[string]any, *schemaValidator)
}

// NonStrict relaxes the tool's schema: fields whose json tag carries
// ",omitempty" stop being required, in the schema advertised and in local
// validation. It returns the tool for chaining; configure it before the tool
// is first used. On a tool not built by NewTool/NewToolNonStrict it only
// clears Strict. It cannot rescue an argument type strict mode cannot express
// at all — NewTool panics first; build those with NewToolNonStrict.
func (t *Tool) NonStrict() *Tool {
	if t.regen != nil {
		t.ParamsJSONSchema, t.validator = t.regen(false)
	}
	t.Strict = false
	return t
}

// needsApproval resolves whether a specific call requires human approval:
// NeedsApprovalFunc when set, NeedsApproval otherwise.
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

// RetrySafeNames returns a predicate for session.RecoveryPolicy.RetrySafe from a set of
// tools, so a caller repairing a session does not have to restate which of its
// tools are safe to repeat.
func RetrySafeNames(tools []*Tool) func(string) bool {
	safe := map[string]bool{}
	for _, t := range tools {
		if t != nil && t.RetrySafe {
			safe[t.Name] = true
		}
	}
	return func(name string) bool { return safe[name] }
}

// DefaultToolErrorFunction is the default FailureErrorFunction: a generic
// model-readable message, with dedicated wording (carrying only the syntax
// error) when the arguments were not decodable JSON.
func DefaultToolErrorFunction(_ context.Context, _ *ToolContext, err error) string {
	if ae, ok := errors.AsType[*toolArgumentsJSONError](err); ok {
		return "An error occurred while parsing tool arguments. Please try again with valid JSON. Error: " + ae.cause.Error()
	}
	return "An error occurred while running the tool. Please try again. Error: " + err.Error()
}
