package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
)

// ToolLoopPolicy bounds the tool loop's safety valves — see spec §2.7d.
type ToolLoopPolicy struct {
	// MaxRepeatedCalls trips the valve when one call (same tool, same
	// arguments) has returned the same output this many times in the run.
	// Zero means 3; negative disables it.
	MaxRepeatedCalls int

	// MaxConsecutiveFailedTurns trips the valve after this many turns in which
	// every tool call failed to run. A result the tool reports, IsError
	// included, is not a failure to run. Zero means 3; negative disables it.
	MaxConsecutiveFailedTurns int

	// Abort makes a trip fail the run with *ToolLoopError, instead of a notice
	// to the model and, on a second trip, a closing turn without tools.
	Abort bool

	// FinalTurnWithoutTools buys an exhausted turn budget one more model call
	// without tools, so the model closes out in prose. Opt-in.
	FinalTurnWithoutTools bool
}

// valveLimit resolves a valve's limit: zero is the default, negative disables.
func valveLimit(v int) int {
	if v == 0 {
		return 3
	}
	return v
}

// ToolLoopError fails a run whose tool loop stopped making progress: a trip
// under ToolLoopPolicy.Abort, or a closing turn that still called a tool.
type ToolLoopError struct {
	// Tool and Repeats describe a repeated call; FailedTurns a run of turns in
	// which no call ran. Exactly one of Repeats and FailedTurns is set.
	Tool        string
	Repeats     int
	FailedTurns int
}

func (e *ToolLoopError) Error() string {
	if e.Repeats > 0 {
		return fmt.Sprintf("tool loop: %s returned the same result %d times for the same arguments", e.Tool, e.Repeats)
	}
	return fmt.Sprintf("tool loop: every tool call failed to run on %d consecutive turns", e.FailedTurns)
}

// toolLoopWindow is how many tool turns a repeat or a notice stays live:
// a call seen again later starts a fresh count — see spec §2.7d.
const toolLoopWindow = 3

// toolLoopState is what the valves have counted; carried in RunState.
type toolLoopState struct {
	// turns counts the tool turns observed; the window is measured in it.
	turns int
	// seen is each call-and-output fingerprint's live count.
	seen map[string]toolLoopRepeat
	// failedTurns counts consecutive turns in which no call ran.
	failedTurns int
	// noticedAt is the tool turn of the last notice; zero is none.
	noticedAt int
	// closing is the second trip: the tools are withdrawn and the next turn is
	// the run's last. Never paused across, so never serialized.
	closing *ToolLoopError
}

// toolLoopRepeat is one fingerprint's count and the tool turn it was last seen.
type toolLoopRepeat struct {
	N, Turn int
}

// carried is the state a pause hands its RunState: a copy, closing dropped.
func (tl toolLoopState) carried() toolLoopState {
	return toolLoopState{turns: tl.turns, seen: maps.Clone(tl.seen), failedTurns: tl.failedTurns, noticedAt: tl.noticedAt}
}

const (
	toolLoopNoticeAdvice = "Do not repeat it: change your approach, or stop and tell the user what you " +
		"have established and what is blocking you."
	toolLoopClosingNotice = "[Runtime notice] The tool loop is still not making progress, so tools are " +
		"withdrawn for the rest of this run. Reply to the user now: what you tried, what you " +
		"established, and what is still open."
)

// toolLoopNotice is the first trip's model-facing notice.
func toolLoopNotice(trip *ToolLoopError) string {
	if trip.Repeats > 0 {
		return fmt.Sprintf("[Runtime notice] This exact call (%s, same arguments) has now returned this "+
			"same result %d times; calling it again will not change it. %s",
			trip.Tool, trip.Repeats, toolLoopNoticeAdvice)
	}
	return fmt.Sprintf("[Runtime notice] Every tool call has failed to run on %d consecutive turns; "+
		"the tools may be unavailable. %s", trip.FailedTurns, toolLoopNoticeAdvice)
}

// noteToolTurn feeds a finished turn's tool results to the valves. A first
// trip appends a notice to the output that tripped it, a second within the
// window withdraws the tools for one closing turn; under Abort, a trip fails
// the run. A batch that ends the run is not counted.
func (r *runner) noteToolTurn(ctx context.Context, results []functionToolResult) error {
	if len(results) == 0 || allTerminate(results) {
		return nil
	}
	tl := &r.toolLoop
	trip, at := tl.observe(results, r.opts.Exec.ToolLoop)
	if trip == nil {
		return nil
	}
	if r.opts.Exec.ToolLoop.Abort {
		return trip
	}
	stage, notice := "notice", toolLoopNotice(trip)
	if tl.noticedAt > 0 && tl.turns-tl.noticedAt <= toolLoopWindow {
		stage, notice = "closing_turn", toolLoopClosingNotice
		tl.closing = trip
		r.state.finalTurn = true
	}
	tl.noticedAt = tl.turns
	results[at].appendNotice(notice)
	RecordDiagnostic(ctx, DiagToolLoop, trip, map[string]any{"stage": stage})
	r.log.Warn(ctx, "tool loop made no progress", slog.String("stage", stage), slog.String("trip", trip.Error()))
	return nil
}

// observe counts one tool turn and reports the first trip and the index of
// the result it lands on; nil when nothing tripped. A fingerprint not seen
// for toolLoopWindow turns starts over, and is forgotten.
func (tl *toolLoopState) observe(results []functionToolResult, p ToolLoopPolicy) (*ToolLoopError, int) {
	maxRepeats := valveLimit(p.MaxRepeatedCalls)
	var trip *ToolLoopError
	at, last, ran := 0, -1, false
	tl.turns++
	for i, res := range results {
		if res.outputItem == nil {
			continue
		}
		last = i
		ran = ran || !res.failed
		if maxRepeats <= 0 {
			continue
		}
		if tl.seen == nil {
			tl.seen = map[string]toolLoopRepeat{}
		}
		key := callFingerprint(res.call, res.output)
		rep := tl.seen[key]
		if tl.turns-rep.Turn > toolLoopWindow {
			rep.N = 0
		}
		rep.N, rep.Turn = rep.N+1, tl.turns
		tl.seen[key] = rep
		if trip == nil && rep.N >= maxRepeats {
			trip, at = &ToolLoopError{Tool: res.call.Name, Repeats: rep.N}, i
		}
	}
	if last < 0 {
		tl.turns--
		return nil, 0
	}
	maps.DeleteFunc(tl.seen, func(_ string, rep toolLoopRepeat) bool { return tl.turns-rep.Turn > toolLoopWindow })
	if ran {
		tl.failedTurns = 0
	} else {
		tl.failedTurns++
	}
	if limit := valveLimit(p.MaxConsecutiveFailedTurns); trip == nil && limit > 0 && tl.failedTurns >= limit {
		trip, at = &ToolLoopError{FailedTurns: tl.failedTurns}, last
	}
	return trip, at
}

// callFingerprint identifies a call and what it returned: tool name, arguments
// as parsed JSON (key order and spacing ignored), and the model-facing output.
func callFingerprint(call functionCall, output any) string {
	h := fnv.New64a()
	h.Write([]byte(call.Name))
	h.Write([]byte{0})
	h.Write(canonicalArguments(call.Arguments))
	h.Write([]byte{0})
	h.Write([]byte(stringifyToolOutput(output)))
	return strconv.FormatUint(h.Sum64(), 16)
}

// canonicalArguments re-encodes JSON arguments with sorted keys and exact
// numbers; anything unparseable is compared as sent.
func canonicalArguments(args string) []byte {
	dec := json.NewDecoder(strings.NewReader(args))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		return []byte(args)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return []byte(args)
	}
	return b
}

// appendNotice adds the runtime's notice to what the model reads of this
// output; the item's Output, and so its display, stays the tool's own.
func (res *functionToolResult) appendNotice(notice string) {
	var out any
	switch v := res.output.(type) {
	case ToolOutputContent:
		out = []ToolOutputContent{v, ToolOutputText{Text: notice}}
	case []ToolOutputContent:
		out = append(slices.Clone(v), ToolOutputText{Text: notice})
	default:
		out = stringifyToolOutput(res.output) + "\n\n" + notice
	}
	res.outputItem.RawInput = newFunctionCallOutputItem(res.outputItem.Agent, res.call.CallID, out).RawInput
}

// toolFreeSettings forbids every tool call while the tools stay on the wire: a
// provider may require the definitions behind the history's calls.
func toolFreeSettings(s *ModelSettings) *ModelSettings {
	var out ModelSettings
	if s != nil {
		out = *s
	}
	out.ToolChoice = ToolChoiceNone
	out.ParallelToolCalls = nil
	return &out
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
		out = append(out, functionToolResult{call: call, output: msg, outputItem: item, failed: true})
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
		out = append(out, functionToolResult{call: call, output: msg, outputItem: item, failed: true})
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
