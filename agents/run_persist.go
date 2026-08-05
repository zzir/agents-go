package agents

import (
	"context"
	"log/slog"

	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/tracing"
)

// persistUserInput writes the run's new user input to the session once, at loop
// start. Later per-turn saves persist only generated items, so the prompt is
// never rewritten. No-op without a session or when there is no new input.
func (r *runner) persistUserInput(ctx context.Context) error {
	if r.opts.Conversation.Session == nil || r.userInputSaved || len(r.userInput) == 0 {
		return nil
	}
	if err := r.opts.Conversation.Session.AppendItems(ctx, r.userInput, Source{Type: SourceUser}); err != nil {
		return err
	}
	r.userInputSaved = true
	return nil
}

// persistSessionItems incrementally saves the sessionItems produced since the
// last save. It persists only the "safe" leading prefix: a trailing
// function_call still awaiting its output (a HITL pause) is held back so the
// stored conversation never contains a call without its output, which would be
// rejected on replay. The held-back calls save on the next turn, once their
// outputs arrive. sessionItems is the unfiltered log, so handoff input filters
// never affect what is stored.
func (r *runner) persistSessionItems(ctx context.Context) error {
	if r.opts.Conversation.Session == nil {
		return nil
	}
	end := safePersistBoundary(r.sessionItems, r.persistedSessionItems)
	if end <= r.persistedSessionItems {
		return nil
	}
	toSave := make([]session.Entry, 0, end-r.persistedSessionItems)
	for _, it := range r.sessionItems[r.persistedSessionItems:end] {
		// Provenance and display ride along, so a reader gets the same timeline
		// the run produced instead of re-deriving it from the wire item.
		e, err := EntryFromRunItem(it, r.lastResponseID)
		if err != nil {
			return err
		}
		toSave = append(toSave, e)
	}
	r.attributeUsage(toSave)
	r.attributeDiagnostics(toSave)
	if len(toSave) > 0 {
		if err := r.opts.Conversation.Session.Append(ctx, toSave...); err != nil {
			return err
		}
		r.log.Debug(ctx, "turn persisted", slog.Int("entries", len(toSave)))
	}
	r.persistedSessionItems = end
	// Injected input taken from the control is delivered once a write has
	// persisted past its position — not before: a safe boundary that stopped
	// short of it (a dangling call pair) leaves it in flight for the next
	// write, or for a rollback if the attempt fails first.
	if r.persistedSessionItems >= r.injectedUpTo {
		r.ctrl.commitInjected()
	}
	return nil
}

// safePersistBoundary returns the exclusive end index up to which items[start:]
// can be safely persisted without ever storing a function_call that lacks its
// matching function_call_output. It returns the largest end such that every
// function_call in items[start:end] has its output also within items[start:end).
//
// Scanning left to right, the boundary advances to just past each point where no
// call is left open (awaiting its output). A pending call — and everything
// ordered after it, including a completed sibling's output that happens to sit
// after it (as at a nested agent-as-tool pause: [call S, call A(pending),
// output S]) — is held back until the missing outputs arrive on resume, so the
// stored history never contains a dangling call. A turn whose calls are all
// paired therefore persists in full.
func safePersistBoundary(items []*RunItem, start int) int {
	if start >= len(items) {
		return len(items)
	}
	end := start
	open := map[string]struct{}{}
	for i := start; i < len(items); i++ {
		id, isCall, isOutput := runItemCallID(items[i])
		switch {
		case isCall:
			open[id] = struct{}{}
		case isOutput:
			delete(open, id)
		}
		if len(open) == 0 {
			end = i + 1
		}
	}
	return end
}

// runItemCallID reports a run item's function-call correlation id and whether it
// is a call or an output, by inspecting its input-item form. Non-function items
// (messages, reasoning, handoffs) report isCall=isOutput=false. Works uniformly
// for live items and items rebuilt from serialized RunState.
func runItemCallID(it *RunItem) (callID string, isCall, isOutput bool) {
	in, err := it.ToInputItem()
	if err != nil {
		return "", false, false
	}
	switch {
	case in.OfFunctionCall != nil:
		return in.OfFunctionCall.CallID, true, false
	case in.OfFunctionCallOutput != nil:
		return in.OfFunctionCallOutput.CallID, false, true
	}
	return "", false, false
}

// compactAfterRun is the CompactAfterRun point: the run's items are persisted
// and its final output produced, so a self-compacting storage gets its turn to
// shrink what it keeps.
//
// It is best-effort housekeeping: a failure is recorded on the trace instead of
// turning a successful run into a failed one.
func (r *runner) compactAfterRun(ctx context.Context) {
	if r.opts.Conversation.Session == nil {
		return
	}
	// A configured Compactor records its result as a checkpoint. It and a
	// self-compacting storage never both apply: compactContext stands aside
	// when the storage compacts itself.
	if r.checkpointAfterRun(ctx) {
		return
	}
	// Items produced locally AFTER the last model response — a final turn's
	// tool/handoff outputs (a terminating tool, rejected calls) or a synthesized
	// error-handler fallback message — are not on the server's
	// previous_response_id chain, so compacting from lastResponseID would
	// erase them from the stored history. With one compaction per run, ending
	// on such a turn means skipping it.
	if endsWithLocalItem(r.sessionItems) {
		return
	}
	if cs, ok := r.opts.Conversation.Session.Storage().(session.CompactionAware); ok {
		// The span starts lazily — only when the session actually compacts —
		// so no-op passes don't clutter the trace.
		var cspan *tracing.SpanHandle
		cerr := cs.RunCompaction(ctx, session.CompactionArgs{
			ResponseID: r.lastResponseID,
			Store:      r.lastStore,
			StartSpan: func() *tracing.SpanHandle {
				cspan = r.trace.StartCompactionSpan(r.agentParentID())
				return cspan
			},
		})
		if cerr != nil && cspan == nil {
			// Failed before the session opened the span; open one so the
			// error is still visible on the trace.
			cspan = r.trace.StartCompactionSpan(r.agentParentID())
		}
		if cspan != nil {
			if cerr != nil {
				cspan.SetError(cerr.Error(), nil)
			}
			cspan.Finish()
		}
	}
}

// endsWithLocalItem reports whether the run's last item was produced locally
// by the SDK rather than returned by the model: a tool/handoff output, or an
// error-handler's synthesized fallback message (marked with the fake response
// id). Such items postdate the last model response and are absent from the
// server-side response chain that previous_response_id compaction replays.
func endsWithLocalItem(items []*RunItem) bool {
	if len(items) == 0 {
		return false
	}
	// Anything the runner synthesized — a tool output, a handoff
	// acknowledgement, an error handler's fallback message — is local. The
	// model's own output and the caller's input are not.
	//
	// This used to be a type switch that string-compared a sentinel id on
	// messages and re-derived the answer from a kind string on restored items;
	// provenance answers it directly, and correctly for item types added later.
	return !items[len(items)-1].Source.IsExternal()
}
