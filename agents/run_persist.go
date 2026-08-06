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
	if cs, ok := r.opts.Conversation.Session.Storage().(session.CompactionAware); ok {
		// The span starts lazily — only when the session actually compacts —
		// so no-op passes don't clutter the trace.
		var cspan *tracing.SpanHandle
		cerr := cs.RunCompaction(ctx, session.CompactionArgs{
			ResponseID: r.lastResponseID,
			Store:      r.lastStore,
			// Whether the log holds anything that response's chain never saw.
			// The storage decides what to do about it: a chain-based one
			// compacts from the stored history instead, one that never looks at
			// a chain ignores it. Deciding here — by skipping the pass — cannot
			// be right in both directions: it would starve a storage that has
			// no chain to be wrong about, and it would miss the items that
			// actually get erased.
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
// A log outgrows the chain in four ways, and only the run knows about all
// four:
//
//   - Items produced after the last model call — hasOffChainItems, a question
//     about POSITION, answered fresh from the run's items every time.
//   - A WINDOWED read (Conversation.Settings.Limit) that truncated: the run
//     sent the model only the newest entries, so anything older is in the log
//     and on no request. Position cannot see it — those entries sit at the
//     FRONT.
//   - A handoff input filter: what it dropped stays in the log and reaches no
//     later model call. Position cannot see that either.
//   - A projector that withholds an ITEM entry — withheldItemEntries. Anywhere
//     in the log, and the only one of the four a caller opts into by config
//     rather than by what the run did.
//
// The last three are recorded as they happen, in offChainHistory, because all
// are facts about the run's past that nothing later can undo.
//
// Only the filter answers conservatively: a filter that RAN sets the flag, with
// no look at what it returned. Telling an identity filter from a real one means
// comparing CONTENT, since one that redacts in place leaves the length
// untouched, and a comparison that got it wrong would fail by deleting the
// original unread — the direction this whole path exists to close. The cost of
// that over-report is bounded and loud: the pass compacts from the stored items
// instead, a larger request that either succeeds or fails visibly.
//
// The window is measured rather than assumed for a reason that does not apply
// to the filter: a filter is per-run, while a window is a permanent setting.
// Answering "a window is configured" would make this permanently true, and a
// caller who pinned a chain-based compaction mode gets their pass SKIPPED on a
// true answer — so they would never compact again, on a log that only grows.
// Past a window that really did truncate, that standoff is real and only the
// caller can end it (unpin the mode, or drop the window); inside one, there is
// nothing to stand off about.
func (r *runner) offChainItems() bool {
	return r.offChainHistory || hasOffChainItems(r.sessionItems)
}

// hasOffChainItems reports whether the run's items include any that POSTDATE
// the last model response.
//
// The last response holds everything that stood in front of the model when it
// answered: its own output, and every tool output, handoff acknowledgement and
// steer that came before it. Those are on the chain, and a summary that folds
// them away read them first. What the chain cannot hold is what came AFTER — a
// terminating tool's output, an error handler's fallback message, input
// injected past the last model call. Position is the whole question here, which
// is why this counts from the last model item rather than asking each item
// where it came from.
//
// It is only ONE of the ways a log outgrows the chain, and the one that clears
// on its own; offChainHistory holds the two that do not, and offChainItems is
// where the three meet.
//
// It answers by provenance in one direction only: SourceModel marks the frontier
// because nothing else can. Provenance alone cannot answer the question — a
// steer taken after the final output is external (the caller wrote it) and yet
// reached no model call.
//
// See withheldItemEntries for the fourth way, which is about configuration
// rather than about what the run did.
func hasOffChainItems(items []*RunItem) bool {
	for i := len(items) - 1; i >= 0; i-- {
		if items[i].Source.Type == SourceModel {
			return i != len(items)-1
		}
	}
	// No model output at all, so nothing anchors these items to a response and
	// none of them can be on its chain.
	return len(items) > 0
}

// withheldItemEntries reports whether the caller's projectors keep an ITEM
// entry out of the model input.
//
// Only item entries can be lost this way. A rewrite carries every other kind
// over verbatim — annotations, terminal output, custom kinds are stored for
// someone other than the model and survive untouched — while item entries are
// exactly what the summary replaces. An item nobody sent is therefore an item
// the summary was written without and the replacement deletes.
//
// A projector that REWRITES an item is not withholding it: the model read
// something in its place, so the summary stands for it, the way a summary
// stands for anything else it folded. Only an empty result means the entry
// reached no request at all, which is what Projector documents returning none
// to mean. That line is what keeps this from being "a projector is installed",
// which never clears — the standoff the window criterion is measured to avoid.
//
// The projector runs a second time here. That is not a new hazard: a run
// already projects the same entries at the save point and again on overflow
// recovery, so one more call cannot be the thing that makes an impure projector
// misbehave.
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
		// An error fails the projection this run is about to do anyway; report
		// it as withheld rather than reading a failure as consent.
		if err != nil || len(items) == 0 {
			return true
		}
	}
	return false
}
