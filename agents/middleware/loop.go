package middleware

import (
	"context"
	"fmt"
	"slices"

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
	// Evaluate judges each attempt. A nil evaluator accepts the first
	// attempt, which makes the middleware a pass-through.
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

			// A stop the caller asked for ends the LOOP, not just the attempt.
			// The stop flag lives on the run control for the whole run and is
			// never cleared, so without this every remaining attempt started,
			// spent one model call and stopped again.
			if res.StoppedEarly {
				break
			}
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
			// what was wrong with it, or it will simply say it again. With a
			// session the attempt is already in the history, so only the
			// feedback goes (spec §2.12).
			feedback := agents.InputItemsFromText(ev.Feedback)
			if in.Opts.Conversation.Session != nil {
				input = feedback
				continue
			}
			prior, ierr := res.ToInputList()
			if ierr != nil {
				yield(nil, fmt.Errorf("middleware: carrying attempt %d forward: %w", attempt, ierr))
				return
			}
			input = slices.Concat(prior, feedback)
		}
		finish(last, yield)
	}
}

var _ agents.RunMiddleware = Loop{}
