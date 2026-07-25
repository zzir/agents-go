package middleware

import (
	"context"
	"fmt"

	"github.com/zzir/agents-go/agents"
)

// Evaluation is an evaluator's verdict on a finished run.
type Evaluation struct {
	// Done ends the loop and reports the run as it stands.
	Done bool
	// Feedback is appended as a user message before the next attempt. It is
	// what makes re-running useful rather than merely repeated: the agent is
	// told what was wrong with the answer it just gave.
	Feedback string
}

// Continue asks for another attempt, telling the agent why.
func Continue(feedback string) Evaluation { return Evaluation{Feedback: feedback} }

// Stop accepts the run's result.
func Stop() Evaluation { return Evaluation{Done: true} }

// Evaluator judges a finished run and says whether to accept it.
type Evaluator func(ctx context.Context, res *agents.RunResult) (Evaluation, error)

// Loop re-runs an agent until an evaluator accepts its answer.
//
// It is the shape middleware exists for: the run loop knows when a model has
// finished talking, and nothing more. "Finished" and "good enough" are
// different questions, and the second one belongs to the caller — a critic
// agent, a schema check, a compiler.
//
// Each attempt streams through, so a caller watching the run sees the rejected
// answers and the feedback, not a long silence followed by the accepted one.
type Loop struct {
	// Evaluate judges each attempt. Required.
	Evaluate Evaluator
	// MaxAttempts bounds the loop. Zero means 3 — an evaluator that never
	// accepts would otherwise run forever on the caller's budget.
	MaxAttempts int
}

// Run implements agents.RunMiddleware.
func (l Loop) Run(ctx context.Context, next agents.RunFunc, in agents.RunInput) agents.RunStream {
	attempts := l.MaxAttempts
	if attempts <= 0 {
		attempts = 3
	}
	return func(yield func(agents.StreamEvent, error) bool) {
		input := in.Input
		var last *agents.RunResult
		for attempt := 1; ; attempt++ {
			turn := in
			turn.Input = input
			res, live, err := collect(next(ctx, turn), yield)
			if !live {
				return
			}
			if err != nil {
				yield(nil, err)
				return
			}
			if res == nil {
				yield(nil, fmt.Errorf("middleware: attempt %d ended without a result", attempt))
				return
			}
			last = res

			if l.Evaluate == nil {
				break
			}
			ev, eerr := l.Evaluate(ctx, res)
			if eerr != nil {
				yield(nil, fmt.Errorf("middleware: evaluating attempt %d: %w", attempt, eerr))
				return
			}
			if ev.Done || attempt >= attempts {
				break
			}
			// Carry the attempt forward: the next run sees what it said and
			// what was wrong with it, or it will simply say it again.
			prior, ierr := res.ToInputList()
			if ierr != nil {
				yield(nil, fmt.Errorf("middleware: carrying attempt %d forward: %w", attempt, ierr))
				return
			}
			input = make([]agents.TResponseInputItem, 0, len(prior)+1)
			input = append(input, prior...)
			input = append(input, agents.InputItemsFromText(ev.Feedback)...)
		}
		finish(last, yield)
	}
}

var _ agents.RunMiddleware = Loop{}
