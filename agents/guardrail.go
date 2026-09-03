package agents

import (
	"context"
	"fmt"
	"runtime/debug"
	"slices"
)

// GuardrailStage identifies where in a run a guardrail is consulted.
type GuardrailStage string

const (
	// StageInput inspects the run's input before the first model call. By
	// default it runs concurrently with that call; set Guardrail.Blocking to
	// make it a gate that runs to completion first.
	StageInput GuardrailStage = "input"
	// StageOutput inspects the final output after the run produces it and
	// before it is persisted.
	StageOutput GuardrailStage = "output"
	// StageToolInput inspects a tool call's arguments before the tool runs.
	StageToolInput GuardrailStage = "tool_input"
	// StageToolOutput inspects a tool's result before it is fed back to the model.
	StageToolOutput GuardrailStage = "tool_output"
)

// GuardrailAction is a guardrail's verdict.
type GuardrailAction int

const (
	// GuardrailAllow lets the run proceed unchanged. The zero value.
	GuardrailAllow GuardrailAction = iota
	// GuardrailReplace substitutes GuardrailDecision.Message for the inspected
	// content and lets the run continue. What gets replaced depends on the
	// stage; see GuardrailDecision.Message.
	GuardrailReplace
	// GuardrailTrip halts the run with a *GuardrailTripwireError.
	GuardrailTrip
)

// GuardrailDecision is what a guardrail returns.
type GuardrailDecision struct {
	// Action is the verdict. The zero value allows.
	Action GuardrailAction
	// Message is the replacement content when Action is GuardrailReplace:
	//
	//   StageInput       the run input is replaced by a single user message
	//                    carrying this text
	//   StageOutput      it becomes the run's final output
	//   StageToolInput   the tool does not execute; this becomes its result
	//   StageToolOutput  it replaces the result sent back to the model
	Message string
	// OutputInfo is arbitrary diagnostic data carried on the result regardless
	// of the action, so callers can inspect why a guardrail decided as it did.
	OutputInfo any
}

// Allow returns an allowing decision carrying optional diagnostic data.
func Allow(outputInfo any) GuardrailDecision {
	return GuardrailDecision{Action: GuardrailAllow, OutputInfo: outputInfo}
}

// Replace returns a decision that substitutes message for the inspected
// content and lets the run continue.
func Replace(message string, outputInfo any) GuardrailDecision {
	return GuardrailDecision{Action: GuardrailReplace, Message: message, OutputInfo: outputInfo}
}

// Trip returns a decision that halts the run with a *GuardrailTripwireError.
func Trip(outputInfo any) GuardrailDecision {
	return GuardrailDecision{Action: GuardrailTrip, OutputInfo: outputInfo}
}

// GuardrailPayload is what a guardrail inspects. Which fields are populated
// depends on Stage:
//
//	StageInput       Input
//	StageOutput      Output
//	StageToolInput   ToolName, ToolCallID, Arguments
//	StageToolOutput  ToolName, ToolCallID, Arguments, Output
//
// Agent is always the agent whose turn is being guarded.
type GuardrailPayload struct {
	Stage GuardrailStage
	Agent *Agent

	// Input is the run input under inspection (StageInput).
	Input []InputItem
	// Output is the value under inspection: the run's final output
	// (StageOutput) or a tool's result (StageToolOutput).
	Output any

	// ToolName, ToolCallID and Arguments describe the tool call under
	// inspection at the tool stages. Arguments is the raw JSON the model emitted.
	ToolName   string
	ToolCallID string
	Arguments  string
}

// Guardrail inspects a run at one or more stages and decides whether to allow,
// substitute, or halt (spec §2.6). One value can cover several stages:
//
//	scanner := agents.Guardrail{
//	    Name:   "pii",
//	    Stages: []agents.GuardrailStage{agents.StageInput, agents.StageToolInput, agents.StageOutput},
//	    Run: func(ctx context.Context, rc *agents.RunContext, p agents.GuardrailPayload) (agents.GuardrailDecision, error) {
//	        ...
//	    },
//	}
//
// For a single stage the typed constructors ([NewInputGuardrail] and friends)
// are shorter. Placement decides scope: guardrails on an [Agent] or in
// [RunOptions] apply to the run, tool stages included; those on a [Tool] to
// that tool only.
type Guardrail struct {
	// Name identifies the guardrail in results and errors.
	Name string
	// Stages lists where this guardrail is consulted. A guardrail with no
	// stages is never run.
	Stages []GuardrailStage
	// Blocking makes a StageInput guardrail run to completion before the first
	// model call — a gate. The zero value races the call and cancels it on a
	// tripwire. No effect at other stages.
	Blocking bool
	// Run inspects the payload and returns a decision.
	Run func(ctx context.Context, rc *RunContext, p GuardrailPayload) (GuardrailDecision, error)
}

// Covers reports whether the guardrail participates in the given stage.
func (g Guardrail) Covers(stage GuardrailStage) bool {
	return slices.Contains(g.Stages, stage)
}

// resolvedName returns Name, or a stable label when unset (Go has no
// function-name reflection to name the callback).
func (g Guardrail) resolvedName() string {
	if g.Name != "" {
		return g.Name
	}
	return "guardrail"
}

// GuardrailResult pairs a guardrail with the decision it made at one stage.
// Every consulted guardrail produces one, including those that allowed, so
// callers can read OutputInfo from all of them.
type GuardrailResult struct {
	Guardrail Guardrail
	Stage     GuardrailStage
	Decision  GuardrailDecision
	// Agent is the agent whose turn was guarded.
	Agent *Agent
	// Checked is the value that was inspected: the run input (StageInput), the
	// final output (StageOutput), or the tool result (StageToolOutput). It is
	// nil at StageToolInput, where Arguments carries the inspected value.
	Checked any
	// ToolName, ToolCallID and Arguments identify the call at the tool stages.
	ToolName   string
	ToolCallID string
	Arguments  string
}

// GuardrailTripwireError is returned when a guardrail trips, at any stage.
type GuardrailTripwireError struct {
	Result GuardrailResult
}

func (e *GuardrailTripwireError) Error() string {
	return fmt.Sprintf("%s guardrail %s tripwire triggered", e.Result.Stage, e.Result.Guardrail.resolvedName())
}

// Stage reports where the tripwire fired, so a caller can branch without
// inspecting the whole result.
func (e *GuardrailTripwireError) Stage() GuardrailStage { return e.Result.Stage }

func newTripwireError(res GuardrailResult) *GuardrailTripwireError {
	return &GuardrailTripwireError{Result: res}
}

// --- typed constructors -----------------------------------------------------

// NewInputGuardrail builds a StageInput guardrail from a callback that sees
// only the input items. Use a [Guardrail] literal when you need the
// [RunContext], the [Agent], or more than one stage.
func NewInputGuardrail(name string, fn func(ctx context.Context, input []InputItem) (GuardrailDecision, error)) Guardrail {
	return Guardrail{
		Name:   name,
		Stages: []GuardrailStage{StageInput},
		Run: func(ctx context.Context, _ *RunContext, p GuardrailPayload) (GuardrailDecision, error) {
			return fn(ctx, p.Input)
		},
	}
}

// NewOutputGuardrail builds a StageOutput guardrail from a callback that sees
// only the final output value.
func NewOutputGuardrail(name string, fn func(ctx context.Context, output any) (GuardrailDecision, error)) Guardrail {
	return Guardrail{
		Name:   name,
		Stages: []GuardrailStage{StageOutput},
		Run: func(ctx context.Context, _ *RunContext, p GuardrailPayload) (GuardrailDecision, error) {
			return fn(ctx, p.Output)
		},
	}
}

// NewToolInputGuardrail builds a StageToolInput guardrail from a callback that
// sees the tool name and its raw JSON arguments.
func NewToolInputGuardrail(name string, fn func(ctx context.Context, toolName, argsJSON string) (GuardrailDecision, error)) Guardrail {
	return Guardrail{
		Name:   name,
		Stages: []GuardrailStage{StageToolInput},
		Run: func(ctx context.Context, _ *RunContext, p GuardrailPayload) (GuardrailDecision, error) {
			return fn(ctx, p.ToolName, p.Arguments)
		},
	}
}

// NewToolOutputGuardrail builds a StageToolOutput guardrail from a callback
// that sees the tool name and its result.
func NewToolOutputGuardrail(name string, fn func(ctx context.Context, toolName string, output any) (GuardrailDecision, error)) Guardrail {
	return Guardrail{
		Name:   name,
		Stages: []GuardrailStage{StageToolOutput},
		Run: func(ctx context.Context, _ *RunContext, p GuardrailPayload) (GuardrailDecision, error) {
			return fn(ctx, p.ToolName, p.Output)
		},
	}
}

// --- execution --------------------------------------------------------------

// selectStage returns the guardrails covering stage, preserving order.
func selectStage(guardrails []Guardrail, stage GuardrailStage) []Guardrail {
	var out []Guardrail
	for _, g := range guardrails {
		if g.Covers(stage) {
			out = append(out, g)
		}
	}
	return out
}

// guardrailPanicError converts a panic recovered from a user callback into an
// error carrying a truncated stack, so a buggy guardrail fails the run only.
func guardrailPanicError(stage GuardrailStage, name string, recovered any) error {
	stack := debug.Stack()
	const maxStack = 4096
	if len(stack) > maxStack {
		stack = append(stack[:maxStack:maxStack], "... (stack truncated)"...)
	}
	return fmt.Errorf("%s guardrail %q panicked: %v\n%s", stage, name, recovered, stack)
}

// runOne invokes a guardrail, recovering panics into errors.
func runOne(ctx context.Context, rc *RunContext, g Guardrail, p GuardrailPayload) (d GuardrailDecision, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = guardrailPanicError(p.Stage, g.resolvedName(), rec)
		}
	}()
	return g.Run(ctx, rc, p)
}

// runStageConcurrent runs every guardrail covering stage concurrently, failing
// fast on the first tripwire or error (spec §2.6); Replace is the caller's to apply.
func runStageConcurrent(ctx context.Context, rc *RunContext, guardrails []Guardrail, p GuardrailPayload) ([]GuardrailResult, error) {
	sel := selectStage(guardrails, p.Stage)
	if len(sel) == 0 {
		return nil, nil
	}
	gctx, cancel := context.WithCancel(ctx)
	defer cancel()

	type outcome struct {
		result GuardrailResult
		err    error
	}
	// Buffered to len(sel): every goroutine can deliver its outcome and exit
	// even when this function has already returned, so none leaks.
	done := make(chan outcome, len(sel))
	for _, g := range sel {
		go func() {
			d, err := runOne(gctx, rc, g, p)
			done <- outcome{result: newGuardrailResult(g, p, d), err: err}
		}()
	}
	results := make([]GuardrailResult, 0, len(sel))
	for range sel {
		oc := <-done
		if oc.err != nil {
			return results, oc.err
		}
		results = append(results, oc.result)
		if oc.result.Decision.Action == GuardrailTrip {
			return results, newTripwireError(oc.result)
		}
	}
	return results, nil
}

// inputReplacement reports the substituted run input when a StageInput
// guardrail returned Replace: the message becomes the whole input (spec §2.6).
func inputReplacement(results []GuardrailResult) ([]InputItem, bool) {
	for _, r := range results {
		if r.Decision.Action == GuardrailReplace {
			return InputItemsFromText(r.Decision.Message), true
		}
	}
	return nil, false
}

// checkedValue is the inspected value recorded on a result; nil at
// StageToolInput, where Arguments carries it.
func checkedValue(p GuardrailPayload) any {
	switch p.Stage {
	case StageInput:
		return p.Input
	case StageOutput, StageToolOutput:
		return p.Output
	default:
		return nil
	}
}

// newGuardrailResult records g's decision d over payload p.
func newGuardrailResult(g Guardrail, p GuardrailPayload, d GuardrailDecision) GuardrailResult {
	return GuardrailResult{
		Guardrail:  g,
		Stage:      p.Stage,
		Decision:   d,
		Agent:      p.Agent,
		Checked:    checkedValue(p),
		ToolName:   p.ToolName,
		ToolCallID: p.ToolCallID,
		Arguments:  p.Arguments,
	}
}

// runStageSequential runs every guardrail covering stage in order, stopping at
// the first Replace or Trip (spec §2.6); it returns the results so far.
func runStageSequential(ctx context.Context, rc *RunContext, guardrails []Guardrail, p GuardrailPayload) (results []GuardrailResult, replacement string, replaced bool, err error) {
	for _, g := range selectStage(guardrails, p.Stage) {
		d, rerr := runOne(ctx, rc, g, p)
		res := newGuardrailResult(g, p, d)
		if rerr != nil {
			return results, "", false, rerr
		}
		results = append(results, res)
		switch d.Action {
		case GuardrailReplace:
			return results, d.Message, true, nil
		case GuardrailTrip:
			return results, "", false, newTripwireError(res)
		}
	}
	return results, "", false, nil
}
