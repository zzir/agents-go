package agents

import "fmt"

// ToolLoopPolicy bounds the tool loop: each valve guards a way an agent can
// keep going and get nowhere, with a default that leaves the ordinary run
// untouched.
type ToolLoopPolicy struct {
	// MaxConsecutiveErrorTurns aborts the run after this many turns in which
	// every tool call failed. Zero means 3; a negative value disables it.
	//
	// It counts TURNS, not calls: a turn where every tool failed increments it,
	// and a turn where any tool succeeded clears it.
	MaxConsecutiveErrorTurns int

	// FinalTurnWithoutTools makes an exhausted turn budget call the model once
	// more with no tools, so it closes out in prose instead of the run failing
	// with a *MaxTurnsError. Opt-in, because it spends a model call the caller's
	// budget said not to spend.
	FinalTurnWithoutTools bool
}

// maxConsecutiveErrorTurns resolves the configured limit. A negative value
// disables the valve.
func (p ToolLoopPolicy) maxConsecutiveErrorTurns() int {
	if p.MaxConsecutiveErrorTurns == 0 {
		return 3
	}
	return p.MaxConsecutiveErrorTurns
}

// ToolLoopError aborts a run whose tools failed on every one of the last N
// turns.
type ToolLoopError struct {
	// Turns is how many consecutive all-failed turns were seen.
	Turns int
}

func (e *ToolLoopError) Error() string {
	return fmt.Sprintf("every tool call failed on %d consecutive turns; aborting rather than "+
		"spending the rest of the turn budget rediscovering the same failure", e.Turns)
}

// noteToolTurn feeds a finished turn's tool results to the consecutive-error
// valve and reports the error when it trips.
//
// A turn with no tool calls at all is not counted either way: the run is
// talking, not looping.
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

// truncatedCallResults fails every tool call in a truncated response without
// running any of them: a response cut off at the output-token limit may have
// half-formed arguments (`{"path": "/ho`), so the model is told to resend
// rather than the call executed as if complete.
func truncatedCallResults(agent *Agent, runs []toolRunFunction) []functionToolResult {
	const msg = "The model response was truncated at the output-token limit, so this tool call's " +
		"arguments may be incomplete. It was NOT executed. Resend the call with complete arguments, " +
		"keeping the response shorter."
	out := make([]functionToolResult, 0, len(runs))
	for _, run := range runs {
		item := newFunctionCallOutputItem(agent, run.Call.CallID, msg)
		item.IsError = true
		out = append(out, functionToolResult{
			callID:     run.Call.CallID,
			tool:       run.Tool,
			output:     msg,
			outputItem: item,
		})
	}
	return out
}

// anySequential reports whether any tool in the batch refuses to run beside
// others.
func anySequential(runs []toolRunFunction) bool {
	for _, run := range runs {
		if run.Tool.Sequential {
			return true
		}
	}
	return false
}

// toolConcurrency resolves how many of a batch's calls may run at once. One
// sequential tool makes the WHOLE batch sequential.
func (r *runner) toolConcurrency(runs []toolRunFunction) int {
	if anySequential(runs) {
		return 1
	}
	return r.opts.Exec.MaxToolConcurrency
}

// discloseTools records the deferred tools this batch's results opened up.
// Disclosure is cumulative for the rest of the run: a tool told about once
// stays available.
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
