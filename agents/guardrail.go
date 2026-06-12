package agents

import "context"

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
}

// OutputGuardrail runs on the agent's final output before the run returns.
type OutputGuardrail struct {
	Name string
	Run  func(ctx context.Context, rc *RunContext, agent *Agent, output any) (GuardrailFunctionOutput, error)
}

// InputGuardrailResult pairs a guardrail with its output.
type InputGuardrailResult struct {
	Guardrail InputGuardrail
	Output    GuardrailFunctionOutput
}

// OutputGuardrailResult pairs a guardrail with its output.
type OutputGuardrailResult struct {
	Guardrail OutputGuardrail
	Output    GuardrailFunctionOutput
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

// runInputGuardrails runs all input guardrails concurrently, returning a
// tripwire error if any trips. It is invoked alongside the first model call.
func runInputGuardrails(ctx context.Context, rc *RunContext, agent *Agent, guardrails []InputGuardrail, input []TResponseInputItem) ([]InputGuardrailResult, error) {
	if len(guardrails) == 0 {
		return nil, nil
	}
	results := make([]InputGuardrailResult, len(guardrails))
	errs := make([]error, len(guardrails))
	done := make(chan int, len(guardrails))
	for i, g := range guardrails {
		go func() {
			out, err := g.Run(ctx, rc, agent, input)
			results[i] = InputGuardrailResult{Guardrail: g, Output: out}
			errs[i] = err
			done <- i
		}()
	}
	for range guardrails {
		<-done
	}
	for i := range guardrails {
		if errs[i] != nil {
			return results, errs[i]
		}
		if results[i].Output.TripwireTriggered {
			return results, &InputGuardrailTripwireError{
				AgentsError: AgentsError{Message: "input guardrail " + guardrails[i].Name + " tripwire triggered"},
				Result:      results[i],
			}
		}
	}
	return results, nil
}

// runOutputGuardrails runs all output guardrails concurrently on the final
// output, returning a tripwire error if any trips.
func runOutputGuardrails(ctx context.Context, rc *RunContext, agent *Agent, guardrails []OutputGuardrail, output any) ([]OutputGuardrailResult, error) {
	if len(guardrails) == 0 {
		return nil, nil
	}
	results := make([]OutputGuardrailResult, len(guardrails))
	errs := make([]error, len(guardrails))
	done := make(chan int, len(guardrails))
	for i, g := range guardrails {
		go func() {
			out, err := g.Run(ctx, rc, agent, output)
			results[i] = OutputGuardrailResult{Guardrail: g, Output: out}
			errs[i] = err
			done <- i
		}()
	}
	for range guardrails {
		<-done
	}
	for i := range guardrails {
		if errs[i] != nil {
			return results, errs[i]
		}
		if results[i].Output.TripwireTriggered {
			return results, &OutputGuardrailTripwireError{
				AgentsError: AgentsError{Message: "output guardrail " + guardrails[i].Name + " tripwire triggered"},
				Result:      results[i],
			}
		}
	}
	return results, nil
}
