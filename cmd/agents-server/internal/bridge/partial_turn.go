package bridge

import (
	"context"
	"errors"
	"time"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/cmd/agents-server/internal/logging"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// A turn that ended before the SDK's per-turn save — cancelled, or failed
// before its first item — is persisted here so a reload still shows it.

// partialTurn is what savePartialTurn writes. Its fields are all strings; build
// it with keyed fields so a misordered pair cannot slip past the compiler.
type partialTurn struct {
	sessionID string
	runID     string
	model     string
	// userInput is the prompt, saved only as a fallback (see savePartialTurn);
	// userAttachments are its image attachment ids.
	userInput       string
	userAttachments []string
	// annRole is the trailing marker's kind, "cancelled" or "error", and annMsg
	// its optional detail. Empty annRole writes no marker.
	annRole string
	annMsg  string
	// partialReasoning and partialText are the in-flight turn's streamed
	// thinking and narration.
	partialReasoning string
	partialText      string
	// guardrail and stage, when set, tag an "error" marker as a guardrail block.
	guardrail string
	stage     string
}

// savePartialTurn records what the SDK cannot for a cancelled or failed run: streamed
// reasoning/text and a stop marker as annotations, plus the prompt if unpersisted.
func (r *Runner) savePartialTurn(t partialTurn) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ref, refErr := store.RefFor(ctx, r.db, t.sessionID)
	if refErr != nil {
		logging.Ctx(r.hub.rootCtx).Warn("persisting partial turn", "error", refErr, "run_id", t.runID, "session_id", t.sessionID)
		return
	}
	es := store.NewEntryStoreFor(r.db, ref)
	es.SetRunID(t.runID)
	es.SetModel(t.model)

	entries := make([]session.Entry, 0, 4)

	if (t.userInput != "" || len(t.userAttachments) > 0) && !runHasPersistedItems(ctx, es, t.runID) {
		for _, item := range (RunInput{Text: t.userInput, AttachmentIDs: t.userAttachments}).items() {
			e, err := session.NewItemEntry(item, agents.Source{Type: agents.SourceUser})
			if err != nil {
				continue
			}
			entries = append(entries, e)
		}
	}

	// Annotations: a fabricated reasoning item would be rejected on replay,
	// and an abandoned turn must not enter the model's history.
	if t.partialReasoning != "" {
		entries = append(entries, session.NewAnnotationEntry(
			agents.ItemDisplay{Kind: agents.DisplayReasoning, Text: t.partialReasoning},
			agents.Source{Type: agents.SourceModel}))
	}
	if t.partialText != "" {
		entries = append(entries, session.NewAnnotationEntry(
			agents.ItemDisplay{Kind: agents.DisplayMessage, Text: t.partialText},
			agents.Source{Type: agents.SourceModel}))
	}

	if t.annRole != "" {
		d := agents.ItemDisplay{Kind: agents.DisplayError, Text: t.annMsg}
		if t.annRole == "cancelled" {
			d.Kind = agents.DisplayCancelled
		}
		// A guardrail block carries its name and stage so a reload rebuilds the
		// typed "Blocked by guardrail X" card instead of a generic error.
		if t.guardrail != "" {
			d.Extra = map[string]any{"guardrail": t.guardrail, "stage": t.stage}
		}
		src := agents.Source{Type: agents.SourceErrorHandler}
		if t.guardrail != "" {
			src = agents.Source{Type: agents.SourceGuardrail}
		}
		entries = append(entries, session.NewAnnotationEntry(d, src))
	}

	if len(entries) == 0 {
		return
	}
	if err := es.Append(ctx, entries...); err != nil {
		// The only durable record of a cancelled/failed turn's prompt and
		// in-flight thinking; best-effort, but never silent.
		logging.Ctx(r.hub.rootCtx).Warn("persisting partial turn", "error", err, "run_id", t.runID, "session_id", t.sessionID)
	}
}

// isCancellation reports whether a run stopped by cancel or deadline rather
// than failing — the run's own ctx, or a context error the provider wrapped.
func isCancellation(ctx context.Context, err error) bool {
	return ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// runHasPersistedItems reports whether the SDK already wrote a replayable item
// row for this run id, so the fallback prompt is not written twice.
func runHasPersistedItems(ctx context.Context, es *store.EntryStore, runID string) bool {
	exists, err := es.RunHasItems(ctx, runID)
	if err != nil {
		// On a query error, assume something was saved: skipping a possibly
		// duplicate prompt is safer than writing a guaranteed duplicate.
		return true
	}
	return exists
}
