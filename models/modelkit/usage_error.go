package modelkit

import "github.com/zzir/agents-go/agents"

// UsageError carries the usage a response billed before the API reported it
// failed or incomplete. errors.As reaches it through any wrapping, and the
// wrapped error's own type still classifies.
type UsageError struct {
	Err   error
	Usage *agents.Usage
}

func (e *UsageError) Error() string { return e.Err.Error() }

func (e *UsageError) Unwrap() error { return e.Err }
