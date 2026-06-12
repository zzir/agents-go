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
}

// AgentsError is the base type for errors raised by the SDK. Use errors.As to
// extract a *AgentsError or one of the concrete error types below.
type AgentsError struct {
	Message string
	Details *RunErrorDetails
}

func (e *AgentsError) Error() string { return e.Message }

// MaxTurnsError is returned when a run exceeds its configured turn budget.
type MaxTurnsError struct {
	AgentsError
	MaxTurns int
}

func newMaxTurnsError(maxTurns int) *MaxTurnsError {
	return &MaxTurnsError{
		AgentsError: AgentsError{Message: fmt.Sprintf("max turns (%d) exceeded", maxTurns)},
		MaxTurns:    maxTurns,
	}
}

// ModelBehaviorError indicates the model did something invalid or unexpected
// (e.g. called a tool that does not exist, or emitted malformed tool calls).
type ModelBehaviorError struct {
	AgentsError
}

func newModelBehaviorError(format string, args ...any) *ModelBehaviorError {
	return &ModelBehaviorError{AgentsError{Message: fmt.Sprintf(format, args...)}}
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
	return &UserError{AgentsError{Message: fmt.Sprintf(format, args...)}}
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
