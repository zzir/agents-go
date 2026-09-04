package agents

import (
	"context"
	"log/slog"
	"slices"

	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/tracing"
)

// persistUserInput writes the run's new user input once, ahead of the first
// model call, and announces it (spec §2.5). No-op without a session or on a resume.
func (r *runner) persistUserInput(ctx context.Context) error {
	if r.opts.Conversation.Session == nil || r.userInputSaved || len(r.userInput) == 0 {
		return nil
	}
	if err := r.opts.Conversation.Session.AppendItems(ctx, r.userInput, Source{Type: SourceUser}); err != nil {
		return err
	}
	r.userInputSaved = true
	if !r.emit(&ItemsPersistedEvent{}) {
		return errConsumerStopped
	}
	return nil
}

// persistSessionItems saves the sessionItems produced since the last save, up
// to safePersistBoundary, from the unfiltered log (spec §2.5).
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
	}
	r.persistedSessionItems = end
	// Injected input commits once a write persisted past its position; a
	// boundary that stopped short leaves it in flight (spec §2.11b).
	if r.persistedSessionItems >= r.injectedUpTo {
		r.ctrl.commitInjected()
	}
	// The store now holds everything the stream has shown —
	// ItemsPersistedEvent's contract. A held-back boundary stays silent.
	if len(toSave) > 0 && end == len(r.sessionItems) && !r.emit(&ItemsPersistedEvent{}) {
		return errConsumerStopped
	}
	return nil
}

// safePersistBoundary returns the exclusive end up to which items[start:] can
// be stored with no function_call lacking its output — spec §2.5.
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

// runItemCallID reports a run item's function-call id and whether it is a
// call or an output; non-function items report neither.
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

// compactAfterRun is the CompactAfterRun point: best-effort housekeeping whose
// failure is recorded on the trace, never turned into a failed run.
func (r *runner) compactAfterRun(ctx context.Context) {
	if r.opts.Conversation.Session == nil {
		return
	}
	// A Compactor checkpoints; a self-compacting storage compacts itself. The
	// two never both apply.
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

// offChainItems reports whether the stored log holds anything the response
// chain rooted at lastResponseID cannot know about — decisions §5.51.
func (r *runner) offChainItems() bool {
	return r.offChainHistory || hasOffChainItems(r.sessionItems)
}

// hasOffChainItems reports whether any item postdates the last model response
// — counted by position from the last SourceModel item (decisions §5.51).
func hasOffChainItems(items []*RunItem) bool {
	for i, item := range slices.Backward(items) {
		if item.Source.Type == SourceModel {
			return i != len(items)-1
		}
	}
	// No model output at all, so nothing anchors these items to a response.
	return len(items) > 0
}

// withheldItemEntries reports whether the projectors keep an ITEM entry out of
// the model input entirely (nil projector, or one returning none).
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
