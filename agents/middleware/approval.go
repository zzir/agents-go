package middleware

import (
	"cmp"
	"context"
	"fmt"

	"github.com/zzir/agents-go/agents"
)

// Decision is what an ApprovalPolicy returns for one pending tool call.
type Decision int

const (
	// Ask leaves the interruption for a human. It is the zero value, so a
	// policy that does not recognize a call defers rather than deciding.
	Ask Decision = iota
	// Allow approves the call.
	Allow
	// Deny rejects it.
	Deny
)

// ApprovalPolicy decides a pending tool call without a human.
//
// Returning Ask for anything it does not recognize is the point: a policy is a
// shortcut for the calls a human has already ruled on, not a replacement for
// asking.
type ApprovalPolicy func(ctx context.Context, item *agents.ToolApprovalItem) (Decision, string)

// AllowTools approves any call to the named tools and defers the rest.
func AllowTools(names ...string) ApprovalPolicy {
	allow := make(map[string]bool, len(names))
	for _, n := range names {
		allow[n] = true
	}
	return func(_ context.Context, item *agents.ToolApprovalItem) (Decision, string) {
		if allow[item.ToolName] {
			return Allow, ""
		}
		return Ask, ""
	}
}

// Approval answers approval interruptions from a policy and resumes the run,
// so a caller only sees the pauses the policy declined to decide.
//
// Standing rules — "always allow read_file", "never allow rm" — are exactly the
// kind of thing that should not be re-litigated by every caller that drives a
// run. The policy runs on the SDK side of the pause, so the caller's loop stays
// "handle the interruptions I was actually asked about".
//
// A run pauses again, unresumed, as soon as the policy returns Ask for any
// call in the batch: an interruption is per-turn, and approving half of one
// while a human decides the rest would run tools the human has not seen yet.
type Approval struct {
	// Policy decides. Required.
	Policy ApprovalPolicy
	// MaxResumes bounds how many times the middleware resumes one run. Zero
	// means 25 — a policy that keeps approving a tool that keeps being called
	// would otherwise loop on the caller's budget.
	MaxResumes int
}

// Run implements agents.RunMiddleware.
func (a Approval) Run(ctx context.Context, next agents.RunFunc, in agents.RunInput) agents.RunStream {
	limit := a.MaxResumes
	if limit <= 0 {
		limit = 25
	}
	return func(yield func(agents.StreamEvent, error) bool) {
		res, live, err := collect(next(ctx, in), yield)
		if !live {
			return
		}
		if err != nil {
			yield(nil, err)
			return
		}

		for resumes := 0; res != nil && len(res.Interruptions) > 0; resumes++ {
			if a.Policy == nil || resumes >= limit {
				break
			}
			if !a.decide(ctx, res) {
				// The policy deferred at least one call, so the pause is real.
				break
			}
			res, live, err = collect(resume(ctx, res.State, in.Opts), yield)
			if !live {
				return
			}
			if err != nil {
				yield(nil, err)
				return
			}
			if res == nil {
				yield(nil, fmt.Errorf("middleware: resumed run ended without a result"))
				return
			}
		}
		finish(res, yield)
	}
}

// decide applies the policy to every pending call, reporting whether all of
// them were settled.
func (a Approval) decide(ctx context.Context, res *agents.RunResult) bool {
	settled := true
	for _, item := range res.Interruptions {
		switch d, msg := a.Policy(ctx, item); d {
		case Allow:
			res.State.Approve(item, false)
		case Deny:
			msg = cmp.Or(msg, "This tool call was rejected by policy.")
			res.State.Reject(item, false, msg)
		default:
			settled = false
		}
	}
	return settled
}

// resume continues a paused run from inside the chain.
//
// Middlewares are stripped: the chain is already unwound at this point, so
// resuming with the run's own options would re-enter this middleware and every
// one outside it. This is a continuation of the run the chain already started,
// not a new one.
func resume(ctx context.Context, state *agents.RunState, opts *agents.RunOptions) agents.RunStream {
	o := *opts
	o.Middlewares = nil
	stream, _ := agents.ResumeRun(ctx, state, o)
	return stream
}

var _ agents.RunMiddleware = Approval{}
