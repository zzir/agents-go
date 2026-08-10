package agents

import (
	"context"
	"errors"
	"time"
)

// Tool is a tool the agent can call: a name and JSON-schema parameters
// shown to the model, plus the Go function the SDK runs when the model calls it.
//
// It is a STRUCT, and it is the only kind of tool there is — deliberately no
// interface, which is how "no hosted tools" is enforced (spec §5.4).
// Everything a tool can do — a timeout, an approval gate, its own guardrails,
// whether it may run concurrently — is a field here, so configuring a tool is
// assigning to it and deriving a variant is copying it:
//
//	gated := *t
//	gated.NeedsApproval = true
//
// A copy shares nothing that matters: the schema map and validator are built
// once and never mutated after construction.
//
// Build one with NewTool, which reflects the argument type into a
// strict-mode schema. The struct is exported so a tool with a hand-written
// schema (NewRawTool, an MCP bridge, a sandbox) can be built directly.
type Tool struct {
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
	// NewTool-built tool use NonStrict, which regenerates both.
	Strict bool
	// OnInvoke runs the tool. argsJSON is the raw JSON arguments string emitted
	// by the model. A tool with no OnInvoke is a configuration error, reported
	// when the model calls it.
	//
	// The result carries everything about the call, not just the model-facing
	// value: UI data that must not reach the model, the renderer to use, token
	// usage the tool spent itself, and whether the run should stop. Use
	// TextResult for the common case.
	OnInvoke func(ctx context.Context, tc *ToolContext, argsJSON string) (ToolResult, error)

	// IsEnabled, when non-nil, is consulted before exposing the tool to the
	// model; returning false hides the tool for that run.
	//
	// To gate a tool you did not build without discarding its own hook, capture
	// the field before overwriting it:
	//
	//	inner := t.IsEnabled
	//	gated := *t
	//	gated.IsEnabled = func(ctx context.Context, rc *RunContext, a *Agent) (bool, error) {
	//	    if !unlocked() { return false, nil }
	//	    if inner != nil { return inner(ctx, rc, a) }
	//	    return true, nil
	//	}
	IsEnabled func(ctx context.Context, rc *RunContext, agent *Agent) (bool, error)

	// Guardrails inspect this tool's calls. Only their tool stages
	// (StageToolInput / StageToolOutput) are consulted; put run-wide guardrails
	// on the Agent instead.
	//
	// Adding to a tool you did not build means appending, not assigning —
	// replacing the slice would disarm the checks the tool declared for itself.
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
	// distinguish concurrent calls to the same tool.
	NeedsApprovalFunc func(ctx context.Context, rc *RunContext, argsJSON string, callID string) (bool, error)

	// FailureErrorFunction controls what happens when OnInvoke returns an error.
	// When non-nil, its returned message is sent back to the model as the tool
	// output, so the model can recover. When nil, the error aborts the run.
	// NewTool installs DefaultToolErrorFunction; set this field to nil
	// to make a tool's errors fatal.
	FailureErrorFunction func(ctx context.Context, tc *ToolContext, err error) string

	// Sequential marks a tool that must not run concurrently with other tools
	// in the same turn. The turn runs it alone, in order.
	Sequential bool

	// Deferred withholds the tool from the model until a ToolResult names it in
	// AddedTools.
	//
	// It is progressive disclosure: an agent offered forty tools chooses worse
	// than one offered four, and most of those forty are only relevant after
	// something else has happened. A tool announcing the tools it unlocks says
	// that directly, where a static list cannot.
	//
	// Once disclosed a tool stays available for the rest of the run; withdrawing
	// it after one use would surprise a model that had just been told it existed.
	Deferred bool

	// RetrySafe declares the tool safe to run again after a crash interrupted it
	// mid-execution. See RetrySafeNames and session.RecoveryPolicy.
	//
	// The default is unsafe, and deliberately so. A process killed between
	// issuing a call and recording its output leaves no way to tell whether the
	// tool ran: the email may already have been sent. Repeating it is a choice
	// only the tool's author can make, and assuming otherwise would make crash
	// recovery a source of duplicate side effects.
	//
	// A reader is a good candidate; anything that writes, charges or notifies is
	// not, unless it is idempotent by construction.
	RetrySafe bool

	// validator is the compiled form of ParamsJSONSchema, used to validate
	// model-sent arguments before they are decoded. Constructors set it
	// together with ParamsJSONSchema so the two cannot drift; a hand-built
	// literal leaves it nil and validates in its own OnInvoke.
	validator *schemaValidator

	// regen rebuilds the schema and validator for a given strictness. NewTool
	// and NewToolNonStrict install it — the closure carries the argument type,
	// which is how NonStrict can re-reflect without a type parameter.
	regen func(strict bool) (map[string]any, *schemaValidator)
}

// NonStrict relaxes the tool's schema: fields whose json tag carries
// ",omitempty" stop being required, in both the schema advertised to the model
// and the local validation of incoming arguments. It returns the tool for
// chaining:
//
//	t := agents.NewTool("get_weather", "look up weather", weatherFn).NonStrict()
//
// Configure it before the tool is first used in a run. On a tool that was not
// built by NewTool or NewToolNonStrict it only clears Strict; the schema stays
// the caller's.
//
// It relaxes a tool that already exists, which is not enough for an argument
// type strict mode cannot express at all (an any/interface{} field, a map with
// arbitrary keys): NewTool panics on those while generating the strict schema,
// before there is anything to relax. Build them with NewToolNonStrict.
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

// DefaultToolErrorFunction is the default FailureErrorFunction: it returns a
// generic, model-readable error message, with dedicated wording when the model
// sent arguments that were not decodable JSON — that message carries only the
// underlying syntax error, prompting the model to retry with valid JSON.
func DefaultToolErrorFunction(_ context.Context, _ *ToolContext, err error) string {
	if ae, ok := errors.AsType[*toolArgumentsJSONError](err); ok {
		return "An error occurred while parsing tool arguments. Please try again with valid JSON. Error: " + ae.cause.Error()
	}
	return "An error occurred while running the tool. Please try again. Error: " + err.Error()
}
