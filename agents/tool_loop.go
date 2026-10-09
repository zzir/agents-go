package agents

import "fmt"

// ToolLoopPolicy bounds the tool loop's safety valves — see spec §2.7d.
type ToolLoopPolicy struct {
	// MaxConsecutiveErrorTurns aborts the run after this many turns in which
	// every tool call failed. Zero means 3; negative disables it.
	MaxConsecutiveErrorTurns int

	// FinalTurnWithoutTools buys an exhausted turn budget one more model call
	// with no tools, so the model closes out in prose. Opt-in.
	FinalTurnWithoutTools bool
}

// maxConsecutiveErrorTurns resolves the configured limit; negative disables the valve.
func (p ToolLoopPolicy) maxConsecutiveErrorTurns() int {
	if p.MaxConsecutiveErrorTurns == 0 {
		return 3
	}
	return p.MaxConsecutiveErrorTurns
}

// ToolLoopError aborts a run whose tools failed on every one of the last N turns.
type ToolLoopError struct {
	// Turns is how many consecutive all-failed turns were seen.
	Turns int
}

func (e *ToolLoopError) Error() string {
	return fmt.Sprintf("every tool call failed on %d consecutive turns; aborting rather than "+
		"spending the rest of the turn budget rediscovering the same failure", e.Turns)
}

// noteToolTurn feeds a turn's tool results to the consecutive-error valve and
// reports the error when it trips; a turn with no tool calls does not count.
func (r *runner) noteToolTurn(results []functionToolResult) error {
	if len(results) == 0 {
		return nil
	}
	for _, res := range results {
		if res.outputItem == nil || !res.outputItem.IsError {
			r.consecutiveErrorTurns = 0
			return nil
		}
	}
	r.consecutiveErrorTurns++
	limit := r.opts.Exec.ToolLoop.maxConsecutiveErrorTurns()
	if limit > 0 && r.consecutiveErrorTurns >= limit {
		return &ToolLoopError{Turns: r.consecutiveErrorTurns}
	}
	return nil
}

// truncatedCallResults answers every call of a truncated response with a
// refusal instead of running it — see spec §2.7e.
func truncatedCallResults(agent *Agent, calls []functionCall) []functionToolResult {
	const msg = "The model response was truncated at the output-token limit, so this tool call's " +
		"arguments may be incomplete. It was NOT executed. Resend the call with complete arguments, " +
		"keeping the response shorter."
	out := make([]functionToolResult, 0, len(calls))
	for _, call := range calls {
		item := newFunctionCallOutputItem(agent, call.CallID, msg)
		item.IsError = true
		out = append(out, functionToolResult{callID: call.CallID, output: msg, outputItem: item})
	}
	return out
}

// unknownCallResults answers every call naming no tool with a not-found error
// output — see spec §2.2 step 7.
func unknownCallResults(agent *Agent, calls []functionCall) []functionToolResult {
	out := make([]functionToolResult, 0, len(calls))
	for _, call := range calls {
		msg := fmt.Sprintf("Tool '%s' not found.", call.Name)
		item := newFunctionCallOutputItem(agent, call.CallID, msg)
		item.IsError = true
		out = append(out, functionToolResult{callID: call.CallID, output: msg, outputItem: item})
	}
	return out
}

// anySequential reports whether any tool in the batch is Sequential.
func anySequential(runs []toolRunFunction) bool {
	for _, run := range runs {
		if run.Tool.Sequential {
			return true
		}
	}
	return false
}

// toolConcurrency resolves how many of a batch's calls may run at once; one
// Sequential tool serializes the whole batch.
func (r *runner) toolConcurrency(runs []toolRunFunction) int {
	if anySequential(runs) {
		return 1
	}
	return r.opts.Exec.MaxToolConcurrency
}

// discloseTools records the deferred tools this batch's results opened up, for
// the rest of the run.
func (r *runner) discloseTools(results []functionToolResult) {
	for _, res := range results {
		for _, name := range res.addedTools {
			if name == "" {
				continue
			}
			if r.disclosed == nil {
				r.disclosed = map[string]bool{}
			}
			r.disclosed[name] = true
		}
	}
}
