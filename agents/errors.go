package agents

import (
	"errors"
	"fmt"
)

// RunErrorDetails carries the run state captured when a run fails, so callers can
// inspect partial progress. It mirrors the Python SDK's RunErrorDetails.
type RunErrorDetails struct {
	Input        []TResponseInputItem
	NewItems     []RunItem
	RawResponses []*ModelResponse
	LastAgent    *Agent
	Usage        *Usage
	// GuardrailResults holds every guardrail result accumulated before the
	// failure, across all stages. Filter by GuardrailResult.Stage.
	GuardrailResults []GuardrailResult
	// Diagnostics is the trouble the run survived before failing. A run that
	// retried three times and then died explains itself here; the error alone
	// only reports the last straw.
	Diagnostics []Diagnostic
}

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

// CodeOf reports the ErrorCode carried by err, unwrapping %w chains. It returns
// CodeUnknown for a nil error, an error the SDK did not produce, or an SDK
// error whose code was never set.
//
// This is the accessor a transport should use. Branching on the concrete error
// types instead means a code added later is invisible to it.
func CodeOf(err error) ErrorCode {
	if err == nil {
		return CodeUnknown
	}
	if base, ok := AsAgentsError(err); ok && base.Code != "" {
		return base.Code
	}
	// Fall back to the concrete type. The SDK's own constructors set Code, but
	// these types are exported — a caller (or a test) building one as a struct
	// literal leaves it zero, and an unclassified GuardrailTripwireError would
	// silently downgrade to generic error handling at the transport. Deriving
	// from the type makes "an SDK error type always classifies" unconditional.
	switch {
	case isType[*GuardrailTripwireError](err):
		return CodeGuardrailTripwire
	case isType[*MaxTurnsError](err):
		return CodeMaxTurns
	case isType[*ModelRefusalError](err):
		return CodeModelRefusal
	case isType[*ModelBehaviorError](err):
		return CodeModelBehavior
	case isType[*ToolTimeoutError](err):
		return CodeToolTimeout
	case isType[*toolPanicError](err):
		// A recovered panic never reaches fatalError, which is where the code
		// used to be attached, so without this a panic the run survived
		// classifies as unknown — exactly the case a diagnostic reports.
		return CodeToolPanic
	case isType[*ToolLoopError](err):
		return CodeToolLoop
	case isType[*UserError](err):
		return CodeUserError
	}
	return CodeUnknown
}

// isType reports whether err's chain contains a T, discarding the value.
func isType[T error](err error) bool {
	_, ok := errors.AsType[T](err)
	return ok
}

// AgentsError is the base type for errors raised by the SDK. Match concrete
// error types with errors.As, or use AsAgentsError to reach the embedded base
// (and its RunErrorDetails) of any SDK error generically. For the failure
// reason alone, prefer CodeOf.
//
//nolint:revive // the name stutters (agents.AgentsError) but the alternative (agents.Error) reads as "the" error type, which it is not.
type AgentsError struct {
	// Code classifies the failure. Zero value means unclassified; CodeOf
	// reports CodeUnknown for it.
	Code    ErrorCode
	Message string
	Details *RunErrorDetails

	// cause is the underlying error, kept in the chain for errors.Is/As.
	// Unexported so it can only be set through Classify, which guarantees the
	// message and the wrapped error stay consistent.
	cause error
}

func (e *AgentsError) Error() string { return e.Message }

// Unwrap exposes the wrapped cause so errors.Is and errors.As see through a
// classification to the original error.
func (e *AgentsError) Unwrap() error { return e.cause }

// Classify tags err with code without hiding it: the result reports code
// through CodeOf while errors.Is and errors.As still reach err itself.
//
// It is how a package outside the run loop (sandbox, mcp, a custom tool)
// contributes a code. Returns nil for a nil err, so it can wrap a return value
// directly. An err that already carries a code is returned unchanged — the
// innermost classification wins, since it knows the most about the failure.
func Classify(code ErrorCode, err error) error {
	if err == nil {
		return nil
	}
	if CodeOf(err) != CodeUnknown {
		return err
	}
	return &AgentsError{Code: code, Message: err.Error(), cause: err}
}

// base lets every error type embedding AgentsError be discovered (and its
// Details populated) through errors.As, even when wrapped.
func (e *AgentsError) base() *AgentsError { return e }

// agentsErrorCarrier is implemented by *AgentsError and, via embedding, by
// every concrete SDK error type.
type agentsErrorCarrier interface {
	error
	base() *AgentsError
}

// AsAgentsError returns the embedded AgentsError of any SDK error in err's
// chain (unwrapping fmt.Errorf %w wrapping). errors.As with **AgentsError
// cannot match the concrete error types, since they embed the base rather
// than wrap it; this helper is the generic accessor.
func AsAgentsError(err error) (*AgentsError, bool) {
	if c, ok := errors.AsType[agentsErrorCarrier](err); ok {
		return c.base(), true
	}
	return nil, false
}

// MaxTurnsError is returned when a run exceeds its configured turn budget.
type MaxTurnsError struct {
	AgentsError
	MaxTurns int
}

func newMaxTurnsError(maxTurns int) *MaxTurnsError {
	return &MaxTurnsError{
		AgentsError: AgentsError{Code: CodeMaxTurns, Message: fmt.Sprintf("max turns (%d) exceeded", maxTurns)},
		MaxTurns:    maxTurns,
	}
}

// ModelBehaviorError indicates the model did something invalid or unexpected
// (e.g. called a tool that does not exist, or emitted malformed tool calls).
type ModelBehaviorError struct {
	AgentsError
}

func newModelBehaviorError(format string, args ...any) *ModelBehaviorError {
	return &ModelBehaviorError{AgentsError{Code: CodeModelBehavior, Message: fmt.Sprintf(format, args...)}}
}

// NewModelBehaviorError constructs a *ModelBehaviorError with a formatted
// message. It is exported so provider packages (e.g. models/openai) can classify
// terminal model failures without importing an unexported constructor.
func NewModelBehaviorError(format string, args ...any) *ModelBehaviorError {
	return newModelBehaviorError(format, args...)
}

// ModelRefusalError indicates the model refused to produce output.
type ModelRefusalError struct {
	AgentsError
	Refusal string
}

// UserError indicates the SDK was used incorrectly (a programming error).
type UserError struct {
	AgentsError
}

func newUserError(format string, args ...any) *UserError {
	return &UserError{AgentsError{Code: CodeUserError, Message: fmt.Sprintf(format, args...)}}
}

// NewUserError constructs a *UserError with a formatted message. It is exported
// so provider packages (e.g. models/openai) can report incorrect SDK usage.
func NewUserError(format string, args ...any) *UserError {
	return newUserError(format, args...)
}

// ToolTimeoutError is returned when a tool invocation exceeds its timeout.
type ToolTimeoutError struct {
	AgentsError
	ToolName string
}

// Sentinel errors for use with errors.Is.
var (
	// ErrMaxTurns matches any MaxTurnsError.
	ErrMaxTurns = errors.New("max turns exceeded")
)

// Is lets errors.Is(err, ErrMaxTurns) match a *MaxTurnsError.
func (e *MaxTurnsError) Is(target error) bool { return target == ErrMaxTurns }

// asAgentsError finds the embedded AgentsError of any SDK error type in err's
// chain (unwrapping fmt.Errorf %w wrapping), so RunErrorDetails can be attached.
func asAgentsError(err error, target **AgentsError) bool {
	if c, ok := errors.AsType[agentsErrorCarrier](err); ok {
		*target = c.base()
		return true
	}
	return false
}
