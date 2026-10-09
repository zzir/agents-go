package agents

import (
	"errors"
	"fmt"
	"time"

	"github.com/zzir/agents-go/agents/session"
)

// ErrorCode is the stable error classification vocabulary, declared in the
// session package — see decisions §5.11.
type ErrorCode = session.ErrorCode

// The codes the SDK produces, re-exported from the session package.
const (
	CodeUnknown           = session.CodeUnknown
	CodeMaxTurns          = session.CodeMaxTurns
	CodeModelBehavior     = session.CodeModelBehavior
	CodeModelRefusal      = session.CodeModelRefusal
	CodeUserError         = session.CodeUserError
	CodeToolTimeout       = session.CodeToolTimeout
	CodeToolLoop          = session.CodeToolLoop
	CodeToolPanic         = session.CodeToolPanic
	CodeGuardrailTripwire = session.CodeGuardrailTripwire
	CodeSandboxExec       = session.CodeSandboxExec
	CodeMCP               = session.CodeMCP
)

// CodeOf reports the ErrorCode carried by err, unwrapping %w chains; CodeUnknown
// for nil or an error the SDK did not produce — see spec §2.10.
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

// codedError attaches an ErrorCode to an error the SDK did not type; built by
// Classify and the panic path, read only through CodeOf.
type codedError struct {
	code  ErrorCode
	msg   string // empty means the cause speaks for itself
	cause error
}

func (e *codedError) Error() string {
	if e.msg != "" {
		return e.msg
	}
	return e.cause.Error()
}

func (e *codedError) Unwrap() error { return e.cause }

// Classify tags err with code without hiding it from errors.Is/As; nil stays
// nil and an already-coded err is returned unchanged — see spec §2.10.
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
// the cause plus the partial progress; an error from before the loop is
// returned bare — see spec §2.10.
//
//	if re, ok := errors.AsType[*agents.RunError](err); ok {
//	    items := re.Result.NewItems // what the run produced before failing
//	}
type RunError struct {
	// Result is the run's partial progress. Never nil; its FinalOutput is nil.
	Result *RunResult
	err    error
}

func (e *RunError) Error() string { return e.err.Error() }

// Unwrap exposes the cause to errors.Is and errors.As.
func (e *RunError) Unwrap() error { return e.err }

// MaxTurnsError is returned when a run exceeds its configured turn budget.
type MaxTurnsError struct {
	MaxTurns int
}

func (e *MaxTurnsError) Error() string {
	return fmt.Sprintf("max turns (%d) exceeded", e.MaxTurns)
}

// ModelBehaviorError indicates the model did something invalid (an unknown
// tool, malformed tool calls).
type ModelBehaviorError struct {
	Message string
}

func (e *ModelBehaviorError) Error() string { return e.Message }

// NewModelBehaviorError constructs a *ModelBehaviorError with a formatted message.
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

// NewUserError constructs a *UserError with a formatted message.
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
