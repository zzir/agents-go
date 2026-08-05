package session

// ErrorCode is the stable, machine-readable classification of an SDK error.
// Unlike a Go type assertion it survives serialization, so a transport (an
// HTTP API, a WebSocket frame, a log line) can carry the reason a run failed
// without the consumer having to parse a message string.
//
// The set is open: a consumer that does not recognize a code must fall back to
// generic handling rather than failing. This is what lets the SDK add a code
// without a coordinated release of everything downstream.
type ErrorCode string

// The codes the SDK produces today. Codes are lowercase snake_case and never
// change once shipped — renaming one silently reclassifies errors for every
// consumer branching on it.
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
