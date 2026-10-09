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

// partialTurn is what savePartialTurn writes; build it with keyed fields (all strings).
type partialTurn struct {
	sessionID string
	runID     string
	model     string
	// userInput is the prompt, saved only as a fallback (see savePartialTurn);
	// userAttachments are its image attachment ids.
	userInput       string
	userAttachments []string
	// injected are queued inputs the run announced as read (run.injected) after
	// its last write; the failed attempt took them back.
	injected []string
	// annRole is the trailing marker's kind ("cancelled" / "error", "" writes
	// none), annMsg its optional detail, code the run.error code.
	annRole string
	annMsg  string
	code    string
	// partialReasoning and partialText are the in-flight turn's streamed
	// thinking and narration.
	partialReasoning string
	partialText      string
	// guardrail and stage, when set, tag an "error" marker as a guardrail block.
	guardrail string
	stage     string
	// notRun are the tool calls an abandoned pause never ran, each written as a
	// call whose display carries not_run: notRunReason (a run.cancelled reason).
	notRun       []store.PendingToolCall
	notRunReason string
}

// savePartialTurn records what the SDK cannot for a cancelled or failed run:
// streamed reasoning/text and a stop marker as annotations, plus any
// unpersisted prompt and injected input.
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

	// An input the run read stays in the transcript; one read at the final
	// output is already written.
	if len(t.injected) > 0 {
		if held, err := es.RunEndsWithUserInputs(ctx, t.runID, t.injected); err != nil || !held {
			for _, text := range t.injected {
				for _, item := range agents.InputItemsFromText(text) {
					e, err := session.NewItemEntry(item, agents.Source{Type: agents.SourceUser})
					if err != nil {
						continue
					}
					entries = append(entries, e)
				}
			}
		}
	}

	// Annotations: an abandoned turn must not enter the model's history.
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
	for _, c := range t.notRun {
		entries = append(entries, session.NewAnnotationEntry(
			agents.ItemDisplay{Kind: agents.DisplayToolCall, CallID: c.ToolCallID, ToolName: c.ToolName, Arguments: c.Arguments,
				Extra: map[string]any{"not_run": t.notRunReason}},
			agents.Source{Type: agents.SourceModel}))
	}

	if t.annRole != "" {
		d := agents.ItemDisplay{Kind: agents.DisplayError, Text: t.annMsg}
		if t.annRole == "cancelled" {
			d.Kind = agents.DisplayCancelled
		}
		// The code and a guardrail's name and stage ride in the extra (a reload
		// rebuilds the card).
		if t.code != "" || t.guardrail != "" {
			d.Extra = map[string]any{}
			if t.code != "" {
				d.Extra["code"] = t.code
			}
			if t.guardrail != "" {
				d.Extra["guardrail"], d.Extra["stage"] = t.guardrail, t.stage
			}
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
		// Best effort, never silent.
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
		// On a query error, assume something was saved (no guaranteed duplicate).
		return true
	}
	return exists
}
