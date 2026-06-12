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

// AgentsError is the base type for errors raised by the SDK. Match concrete
// error types with errors.As, or use AsAgentsError to reach the embedded base
// (and its RunErrorDetails) of any SDK error generically.
type AgentsError struct {
	Message string
	Details *RunErrorDetails
}

func (e *AgentsError) Error() string { return e.Message }

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
