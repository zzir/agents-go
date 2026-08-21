package agents

import (
	"context"
	"log/slog"
	"slices"

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
// last save. It persists only the safe leading prefix — a trailing output-less
// function_call (a HITL pause) is held back for the next turn, so the stored
// conversation never has a call without its output — from the unfiltered log,
// so handoff input filters never affect what is stored.
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
		if end == len(r.sessionItems) {
			// The store now holds everything the stream has shown —
			// ItemsPersistedEvent's contract. A held-back boundary stays silent.
			_ = r.emit(&ItemsPersistedEvent{})
		}
	}
	r.persistedSessionItems = end
	// Injected input commits once a write has persisted past its position; a
	// boundary that stopped short leaves it in flight for the next write or a
	// rollback.
	if r.persistedSessionItems >= r.injectedUpTo {
		r.ctrl.commitInjected()
	}
	return nil
}

// safePersistBoundary returns the exclusive end index up to which items[start:]
// can be safely persisted without ever storing a function_call that lacks its
// matching function_call_output: the largest end where every call in
// items[start:end] has its output within items[start:end) too.
//
// The boundary advances past each point where no call is left open; a pending
// call and everything ordered after it is held back until its output arrives on
// resume. A turn whose calls are all paired persists in full.
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
// report isCall=isOutput=false. Works for live and rebuilt items alike.
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
	if cs, ok := r.opts.Conversation.Session.Storage().(session.CompactionAware); ok {
		// The span starts lazily — only when the session actually compacts —
		// so no-op passes don't clutter the trace.
		var cspan *tracing.SpanHandle
		cerr := cs.RunCompaction(ctx, session.CompactionArgs{
			ResponseID: r.lastResponseID,
			Store:      r.lastStore,
			// Whether the log holds anything that response's chain never saw;
			// the storage decides what to do about it.
			OffChainItems: r.offChainItems(),
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

// offChainItems reports whether the stored log holds anything the server-side
// response chain rooted at lastResponseID cannot know about — what a storage
// compacting from that chain would delete without ever having read.
//
// A log outgrows the chain four ways: items produced after the last model call
// (hasOffChainItems, answered fresh from position); a windowed read that
// truncated; a handoff input filter's drops; and a projector that withholds an
// item entry (withheldItemEntries). The last three are recorded in
// offChainHistory as they happen. The filter answers conservatively — running
// sets the flag — so at worst the pass over-reports and compacts from the
// stored items, a larger request that succeeds or fails visibly.
func (r *runner) offChainItems() bool {
	return r.offChainHistory || hasOffChainItems(r.sessionItems)
}

// hasOffChainItems reports whether the run's items include any that postdate the
// last model response — a terminating tool's output, an error handler's
// fallback, input injected past the last model call. It counts from the last
// SourceModel item because position, not provenance, is the whole question, and
// it is the one way a log outgrows the chain that clears on its own.
func hasOffChainItems(items []*RunItem) bool {
	for i, item := range slices.Backward(items) {
		if item.Source.Type == SourceModel {
			return i != len(items)-1
		}
	}
	// No model output at all, so nothing anchors these items to a response.
	return len(items) > 0
}

// withheldItemEntries reports whether the caller's projectors keep an ITEM entry
// out of the model input entirely — a nil projector, or one returning none.
// Only item entries can be lost this way: a rewrite carries every other kind
// over verbatim, and the summary that replaces items is written without any the
// projector withheld. A projector that REWRITES an item is not withholding it.
func withheldItemEntries(entries []session.Entry, projectors map[session.EntryKind]session.Projector) bool {
	project, overridden := projectors[session.EntryKindItem]
	if !overridden {
		// The default projection sends every item entry.
		return false
	}
	for _, e := range entries {
		if e.Kind != session.EntryKindItem {
			continue
		}
		// A nil projector suppresses the kind outright, so the first item entry
		// settles it.
		if project == nil {
			return true
		}
		items, err := project(e)
		// An error fails the projection anyway; report it as withheld, not consent.
		if err != nil || len(items) == 0 {
			return true
		}
	}
	return false
}
