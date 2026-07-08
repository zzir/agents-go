package agents

import (
	"context"
	"fmt"
	"runtime/debug"
)

// GuardrailFunctionOutput is the result a guardrail function returns. If
// TripwireTriggered is true the run is halted with a tripwire error.
type GuardrailFunctionOutput struct {
	// OutputInfo is arbitrary diagnostic data attached to the result.
	OutputInfo any
	// TripwireTriggered halts the run when true.
	TripwireTriggered bool
}

// InputGuardrail runs on the original input before (in parallel with) the first
// model call. It mirrors the Python SDK's InputGuardrail.
type InputGuardrail struct {
	// Name identifies the guardrail in results and errors.
	Name string
	// Run inspects the input and returns a tripwire decision.
	Run func(ctx context.Context, rc *RunContext, agent *Agent, input []TResponseInputItem) (GuardrailFunctionOutput, error)
	// Blocking, when true, runs this guardrail to completion BEFORE the first
	// model call — a gate: a tripwire prevents the call and any token spend. The
	// zero value (false) runs it concurrently with the model call, the default.
	// This is the inverse of Python's InputGuardrail.run_in_parallel (whose
	// default True can't be a Go bool zero value): Blocking == !run_in_parallel.
	Blocking bool
}

// OutputGuardrail runs on the agent's final output before the run returns.
type OutputGuardrail struct {
	Name string
	Run  func(ctx context.Context, rc *RunContext, agent *Agent, output any) (GuardrailFunctionOutput, error)
}

// resolvedName returns the guardrail's Name, falling back to a non-empty default
// when it is unset. Go has no function-name reflection, so unlike the Python SDK
// (which uses the guardrail function's __name__) the fallback is a fixed label.
func (g InputGuardrail) resolvedName() string {
	if g.Name != "" {
		return g.Name
	}
	return "input_guardrail"
}

// resolvedName returns the guardrail's Name, falling back to a non-empty default
// when it is unset (see InputGuardrail.resolvedName).
func (g OutputGuardrail) resolvedName() string {
	if g.Name != "" {
		return g.Name
	}
	return "output_guardrail"
}

// InputGuardrailResult pairs a guardrail with its output.
type InputGuardrailResult struct {
	Guardrail InputGuardrail
	Output    GuardrailFunctionOutput
}

// OutputGuardrailResult pairs a guardrail with its output. Agent and AgentOutput
// record which agent produced the checked output and the output itself (Python
// parity), so a caller reading RunResult.OutputGuardrailResults — or catching
// an OutputGuardrailTripwireError — can inspect what was flagged.
type OutputGuardrailResult struct {
	Guardrail   OutputGuardrail
	Output      GuardrailFunctionOutput
	Agent       *Agent
	AgentOutput any
}

// InputGuardrailTripwireError is returned when an input guardrail trips.
type InputGuardrailTripwireError struct {
	AgentsError
	Result InputGuardrailResult
}

// OutputGuardrailTripwireError is returned when an output guardrail trips.
type OutputGuardrailTripwireError struct {
	AgentsError
	Result OutputGuardrailResult
}

// guardrailPanicError converts a panic recovered from a user guardrail callback
// into an error carrying a truncated stack trace, so a buggy guardrail fails the
// run instead of crashing the process.
func guardrailPanicError(kind, name string, recovered any) error {
	stack := debug.Stack()
	const maxStack = 4096
	if len(stack) > maxStack {
		stack = append(stack[:maxStack:maxStack], "... (stack truncated)"...)
	}
	return fmt.Errorf("%s guardrail %q panicked: %v\n%s", kind, name, recovered, stack)
}

// runInputGuardrails runs all input guardrails concurrently. It fails fast,
// matching the Python SDK: the first tripwire or error returned by any
// guardrail ends the wait immediately and cancels the context passed to the
// remaining guardrails. A panic inside a guardrail callback is recovered and
// reported as that guardrail's error. It is invoked alongside the first model
// call.
func runInputGuardrails(ctx context.Context, rc *RunContext, agent *Agent, guardrails []InputGuardrail, input []TResponseInputItem) ([]InputGuardrailResult, error) {
	if len(guardrails) == 0 {
		return nil, nil
	}
	// Canceled on early return so still-running guardrails can stop promptly.
	gctx, cancel := context.WithCancel(ctx)
	defer cancel()

	type outcome struct {
		result InputGuardrailResult
		err    error
	}
	// Buffered to len(guardrails): every goroutine can deliver its outcome and
	// exit even when this function has already returned, so none leaks.
	done := make(chan outcome, len(guardrails))
	for _, g := range guardrails {
		go func() {
			defer func() {
				if rec := recover(); rec != nil {
					done <- outcome{err: guardrailPanicError("input", g.Name, rec)}
				}
			}()
			out, err := g.Run(gctx, rc, agent, input)
			done <- outcome{result: InputGuardrailResult{Guardrail: g, Output: out}, err: err}
		}()
	}
	results := make([]InputGuardrailResult, 0, len(guardrails))
	for range guardrails {
		oc := <-done
		if oc.err != nil {
			return results, oc.err
		}
		results = append(results, oc.result)
		if oc.result.Output.TripwireTriggered {
			return results, &InputGuardrailTripwireError{
				AgentsError: AgentsError{Message: "input guardrail " + oc.result.Guardrail.resolvedName() + " tripwire triggered"},
				Result:      oc.result,
			}
		}
	}
	return results, nil
}

// runOutputGuardrails runs all output guardrails concurrently on the final
// output. Like runInputGuardrails it fails fast on the first tripwire or error
// (canceling the context handed to the remaining guardrails) and converts a
// guardrail panic into that guardrail's error.
func runOutputGuardrails(ctx context.Context, rc *RunContext, agent *Agent, guardrails []OutputGuardrail, output any) ([]OutputGuardrailResult, error) {
	if len(guardrails) == 0 {
		return nil, nil
	}
	gctx, cancel := context.WithCancel(ctx)
	defer cancel()

	type outcome struct {
		result OutputGuardrailResult
		err    error
	}
	done := make(chan outcome, len(guardrails))
	for _, g := range guardrails {
		go func() {
			defer func() {
				if rec := recover(); rec != nil {
					done <- outcome{err: guardrailPanicError("output", g.Name, rec)}
				}
			}()
			out, err := g.Run(gctx, rc, agent, output)
			done <- outcome{result: OutputGuardrailResult{Guardrail: g, Output: out, Agent: agent, AgentOutput: output}, err: err}
		}()
	}
	results := make([]OutputGuardrailResult, 0, len(guardrails))
	for range guardrails {
		oc := <-done
		if oc.err != nil {
			return results, oc.err
		}
		results = append(results, oc.result)
		if oc.result.Output.TripwireTriggered {
			return results, &OutputGuardrailTripwireError{
				AgentsError: AgentsError{Message: "output guardrail " + oc.result.Guardrail.resolvedName() + " tripwire triggered"},
				Result:      oc.result,
			}
		}
	}
	return results, nil
}

// NewInputGuardrail creates an InputGuardrail with a simplified callback that
// receives only the input items. Use the full InputGuardrail struct literal when
// you need access to the RunContext or Agent.
func NewInputGuardrail(name string, fn func(input []TResponseInputItem) (GuardrailFunctionOutput, error)) InputGuardrail {
	return InputGuardrail{
		Name: name,
		Run: func(_ context.Context, _ *RunContext, _ *Agent, input []TResponseInputItem) (GuardrailFunctionOutput, error) {
			return fn(input)
		},
	}
}

// NewOutputGuardrail creates an OutputGuardrail with a simplified callback that
// receives only the output value. Use the full OutputGuardrail struct literal
// when you need access to the RunContext or Agent.
func NewOutputGuardrail(name string, fn func(output any) (GuardrailFunctionOutput, error)) OutputGuardrail {
	return OutputGuardrail{
		Name: name,
		Run: func(_ context.Context, _ *RunContext, _ *Agent, output any) (GuardrailFunctionOutput, error) {
			return fn(output)
		},
	}
}
