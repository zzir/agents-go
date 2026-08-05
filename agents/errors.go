package agents

import (
	"errors"
	"fmt"
	"time"
)

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
// CodeUnknown for a nil error or one the SDK did not produce.
//
// The code is DERIVED from the error's type, so there is exactly one source of
// truth: an SDK error cannot be built with a mismatched code, because there is
// no code to set. (A previous design carried a Code field beside the types, and
// the two disagreed exactly as often as a constructor was bypassed.)
//
// This is the accessor a transport should use. Branching on the concrete error
// types instead means a code added later is invisible to it.
func CodeOf(err error) ErrorCode {
	if err == nil {
		return CodeUnknown
	}
	if ce, ok := errors.AsType[*codedError](err); ok {
		return ce.code
	}
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

// codedError attaches an ErrorCode to an error the SDK did not type — how a
// package outside the run loop (sandbox, mcp, a custom tool) contributes a
// classification. Built by Classify (and by the panic path, which carries a
// message of its own); read only through CodeOf.
type codedError struct {
	code  ErrorCode
	msg   string // optional; empty means the cause speaks for itself
	cause error
}

func (e *codedError) Error() string {
	if e.msg != "" {
		return e.msg
	}
	return e.cause.Error()
}

func (e *codedError) Unwrap() error { return e.cause }

// Classify tags err with code without hiding it: the result reports code
// through CodeOf while errors.Is and errors.As still reach err itself.
//
// Returns nil for a nil err, so it can wrap a return value directly. An err
// that already carries a code is returned unchanged — the innermost
// classification wins, since it knows the most about the failure.
func Classify(code ErrorCode, err error) error {
	if err == nil {
		return nil
	}
	if CodeOf(err) != CodeUnknown {
		return err
	}
	return &codedError{code: code, cause: err}
}

// RunError is the terminal error of a run that failed after its loop started:
// the cause, plus everything the run produced before failing.
//
// Result carries the partial progress — input, generated items, raw responses,
// usage, guardrail results, diagnostics — as a *RunResult with a nil
// FinalOutput. It is the same shape a completed run reports, because a failed
// run and a finished one describe the same thing; the difference is that one
// has an answer. (A previous design duplicated seven RunResult fields into a
// separate details struct, and attached it only when the cause happened to be
// an SDK-typed error — a plain error from a hook or the session lost the
// progress entirely.)
//
// Reach it with errors.AsType; classify the cause with CodeOf, which sees
// through this wrapper:
//
//	if re, ok := errors.AsType[*agents.RunError](err); ok {
//	    items := re.Result.NewItems // what the run produced before failing
//	}
//
// Errors from before the loop — a bad option combination, an unresolvable
// model — are returned bare: there is no progress to report.
type RunError struct {
	// Result is the run's partial progress. Never nil; its FinalOutput is nil.
	Result *RunResult
	err    error
}

func (e *RunError) Error() string { return e.err.Error() }

// Unwrap exposes the cause, so errors.Is and errors.As see through the wrapper.
func (e *RunError) Unwrap() error { return e.err }

// MaxTurnsError is returned when a run exceeds its configured turn budget.
type MaxTurnsError struct {
	MaxTurns int
}

func (e *MaxTurnsError) Error() string {
	return fmt.Sprintf("max turns (%d) exceeded", e.MaxTurns)
}

// ModelBehaviorError indicates the model did something invalid or unexpected
// (e.g. called a tool that does not exist, or emitted malformed tool calls).
type ModelBehaviorError struct {
	Message string
}

func (e *ModelBehaviorError) Error() string { return e.Message }

// NewModelBehaviorError constructs a *ModelBehaviorError with a formatted
// message. It is exported so provider packages (e.g. models/openai) can
// classify terminal model failures.
func NewModelBehaviorError(format string, args ...any) *ModelBehaviorError {
	return &ModelBehaviorError{Message: fmt.Sprintf(format, args...)}
}

// ModelRefusalError indicates the model refused to produce output.
type ModelRefusalError struct {
	Refusal string
}

func (e *ModelRefusalError) Error() string {
	return "model refused to respond: " + e.Refusal
}

// UserError indicates the SDK was used incorrectly (a programming error).
type UserError struct {
	Message string
}

func (e *UserError) Error() string { return e.Message }

// NewUserError constructs a *UserError with a formatted message. It is exported
// so provider packages (e.g. models/openai) can report incorrect SDK usage.
func NewUserError(format string, args ...any) *UserError {
	return &UserError{Message: fmt.Sprintf(format, args...)}
}

// ToolTimeoutError is returned when a tool invocation exceeds its timeout.
type ToolTimeoutError struct {
	ToolName string
	Timeout  time.Duration
}

func (e *ToolTimeoutError) Error() string {
	return fmt.Sprintf("tool %q timed out after %v", e.ToolName, e.Timeout)
}
