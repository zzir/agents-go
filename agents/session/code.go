package session

// ErrorCode is the stable, machine-readable classification of an SDK error;
// it survives serialization, so a transport can carry why a run failed. The
// set is open: an unrecognized code gets generic handling, never a failure.
type ErrorCode string

// The codes the SDK produces: lowercase snake_case, never changed once shipped
// — renaming one reclassifies errors for every consumer branching on it.
const (
	// CodeUnknown is what CodeOf reports for an error the SDK did not classify,
	// including a plain error from user code.
	CodeUnknown ErrorCode = "unknown"

	CodeMaxTurns          ErrorCode = "max_turns_exceeded"
	CodeModelBehavior     ErrorCode = "model_behavior"
	CodeModelRefusal      ErrorCode = "model_refusal"
	CodeUserError         ErrorCode = "user_error"
	CodeToolTimeout       ErrorCode = "tool_timeout"
	CodeToolLoop          ErrorCode = "tool_loop"
	CodeToolPanic         ErrorCode = "tool_panic"
	CodeGuardrailTripwire ErrorCode = "guardrail_tripwire"
	CodeSandboxExec       ErrorCode = "sandbox_exec"
	CodeMCP               ErrorCode = "mcp"
)
