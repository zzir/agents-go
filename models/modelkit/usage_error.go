package modelkit

import "github.com/zzir/agents-go/agents"

// UsageError carries the usage a response billed before the API reported it
// failed or incomplete, so the tokens a run never received are not lost from
// its accounting. errors.As reaches it through any wrapping; errors.As on the
// wrapped error's own type still works, so classification is unchanged.
type UsageError struct {
	Err   error
	Usage *agents.Usage
}

func (e *UsageError) Error() string { return e.Err.Error() }

func (e *UsageError) Unwrap() error { return e.Err }
