package agents

import (
	"fmt"
	"sort"
)

// ToolLoopPolicy bounds the tool loop.
//
// The loop's failure modes are not the model's ordinary mistakes — they are the
// ones where an agent keeps going and gets nowhere: the same tool failing every
// turn, or a turn budget that runs out mid-sentence. Each valve here is one of
// those, with a default that leaves the ordinary run untouched.
type ToolLoopPolicy struct {
	// MaxConsecutiveErrorTurns aborts the run after this many turns in which
	// every tool call failed. Zero means 3; a negative value disables it.
	//
	// It counts TURNS, not calls: one turn where every tool failed increments
	// it, and a turn where any tool succeeded clears it. A model stuck calling
	// a broken tool would otherwise burn the whole turn budget rediscovering
	// that it is broken, and bill for it.
	MaxConsecutiveErrorTurns int

	// FinalTurnWithoutTools makes an exhausted turn budget call the model once
	// more with no tools, so it closes out in prose instead of the run failing
	// with a *MaxTurnsError.
	//
	// It is opt-in rather than the default because it spends a model call the
	// caller's budget said not to spend. When the budget is a cost ceiling that
	// is wrong; when it is a guard against looping, an answer beats an error.
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
	AgentsError
	// Turns is how many consecutive all-failed turns were seen.
	Turns int
}

func newToolLoopError(turns int) *ToolLoopError {
	return &ToolLoopError{
		AgentsError: AgentsError{
			Code: CodeToolLoop,
			Message: fmt.Sprintf("every tool call failed on %d consecutive turns; aborting rather than "+
				"spending the rest of the turn budget rediscovering the same failure", turns),
		},
		Turns: turns,
	}
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
		return newToolLoopError(r.consecutiveErrorTurns)
	}
	return nil
}

// truncatedCallResults fails every tool call in a truncated response without
// running any of them.
//
// A response cut off at the output-token limit looks ordinary — items present,
// no error — but its tail may be half-formed, and a tool call's arguments are
// exactly the kind of tail that gets cut. Executing `{"path": "/ho` as if it
// were complete is how an agent acts on something nobody asked for. The model
// is told what happened so it can resend, which it can only do if it is told
// rather than shown a plausible-looking result.
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

// toolConcurrency resolves how many of a batch's calls may run at once.
//
// One sequential tool makes the WHOLE batch sequential. Per-tool serialization
// would be finer, but a tool that says "do not run me beside anything" usually
// means it for a resource — a shell session, a working directory — that the
// other tools in the batch touch too.
func (r *runner) toolConcurrency(runs []toolRunFunction) int {
	if anySequential(runs) {
		return 1
	}
	return r.opts.Exec.MaxToolConcurrency
}

// discloseTools records the deferred tools this batch's results opened up.
//
// Disclosure is cumulative for the rest of the run: a tool told about once
// stays available. Withdrawing it after a single use would surprise a model
// that had just been told it existed.
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

// sortedKeys returns a set's members in a stable order, so serialized state
// does not churn between otherwise identical runs.
func sortedKeys(m map[string]bool) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
