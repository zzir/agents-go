package store

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"

	"github.com/uptrace/bun"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/compaction"
	"github.com/zzir/agents-go/tracing"
)

// CompactionNotifier receives compaction lifecycle notifications. OnStart
// fires right before the (potentially slow) summarization request, so the UI
// can tell the user why the run is still busy; OnDone fires after a
// successful compaction with the item counts before and after.
type CompactionNotifier struct {
	OnStart func()
	OnDone  func(before, after int)
}

// CompactionAdapter wraps an EntryStore with provider-agnostic compaction that
// soft-deletes folded entries (marks them compacted=true) rather than removing
// them, so the agents-server UI can still show what was folded away.
type CompactionAdapter struct {
	*EntryStore
	summaryModel  agents.Model
	threshold     int
	windowSize    int
	summaryPrompt string
	notify        CompactionNotifier
}

var (
	_ agents.SessionStorage  = (*CompactionAdapter)(nil)
	_ agents.CompactionAware = (*CompactionAdapter)(nil)
)

// NewCompactionAdapter wraps store with soft-delete compaction.
func NewCompactionAdapter(
	store *EntryStore,
	summaryModel agents.Model,
	threshold, windowSize int,
	summaryPrompt string,
	notify CompactionNotifier,
) *CompactionAdapter {
	if threshold <= 0 {
		threshold = 20
	}
	if windowSize <= 0 {
		windowSize = 10
	}
	summaryPrompt = cmp.Or(summaryPrompt, agents.DefaultSummaryPrompt)
	return &CompactionAdapter{
		EntryStore:    store,
		summaryModel:  summaryModel,
		threshold:     threshold,
		windowSize:    windowSize,
		summaryPrompt: summaryPrompt,
		notify:        notify,
	}
}

// RunCompaction implements agents.CompactionAware. It marks older entries
// compacted and appends a compaction checkpoint, keeping the most recent
// windowSize non-compacted entries intact.
func (ca *CompactionAdapter) RunCompaction(ctx context.Context, args agents.CompactionArgs) error {
	var active []entryRow
	if err := ca.db.NewSelect().Model(&active).
		Where("session_id = ?", ca.sessionID).
		Where("compacted = ?", false).
		OrderExpr("id ASC").
		Scan(ctx); err != nil {
		return fmt.Errorf("compaction adapter: loading active entries: %w", err)
	}

	if !args.Force && len(active)-ca.windowSize < ca.threshold {
		return nil
	}

	if ca.windowSize >= len(active) {
		return nil
	}

	// Convert every active entry to its replayable item, remembering which row
	// each item came from. Rows that don't convert (annotations, reasoning items
	// dropped for foreign replay, malformed JSON) carry no pairing constraints;
	// the row-split mapping below leaves them on the same side as their
	// preceding convertible neighbor.
	items := make([]agents.TResponseInputItem, 0, len(active))
	entries := make([]agents.SessionEntry, 0, len(active))
	itemMsgIdx := make([]int, 0, len(active))
	for i := range active {
		var e agents.SessionEntry
		if json.Unmarshal([]byte(active[i].Entry), &e) != nil {
			continue
		}
		if e.Kind != agents.EntryKindItem || len(e.Item) == 0 {
			continue
		}
		// The summary model is generally not the model that produced these
		// items, so always adapt them for foreign replay (drop reasoning
		// items, strip provider-assigned ids) before summarizing.
		raw := adaptForeignItemJSON(e.Item)
		if raw == nil {
			continue
		}
		normalized := NormalizeItemJSON(raw)
		item, err := agents.UnmarshalInputItem(normalized)
		if err != nil {
			continue
		}
		items = append(items, item)
		// The grouping below reads the wire JSON, so carry it alongside rather
		// than re-marshaling what we just decoded.
		entries = append(entries, agents.SessionEntry{Kind: agents.EntryKindItem, Item: normalized})
		itemMsgIdx = append(itemMsgIdx, i)
	}

	// The count-based split in row space, translated to item space.
	msgSplit := len(active) - ca.windowSize
	itemSplit := 0
	for itemSplit < len(items) && itemMsgIdx[itemSplit] < msgSplit {
		itemSplit++
	}
	if itemSplit == 0 {
		return nil // nothing summarizable below the window
	}

	// A pure count-based split can cut through a function_call / output pair,
	// making the summary request itself invalid or leaving the kept history
	// starting with an orphaned output. Snap the split to a group boundary so
	// both sides stay self-consistent; 0 means no valid non-empty prefix
	// exists, so skip this pass rather than risk corrupting the history.
	itemSplit = compaction.SafeSplit(entries, itemSplit)
	if itemSplit <= 0 {
		return nil
	}
	toSummarize := items[:itemSplit]

	// Summarizing a summary produces a paraphrase of a paraphrase.
	if compaction.IsSummaryOnly(entries[:itemSplit]) {
		return nil
	}

	// Map the safe item split back to row space: when the split moved,
	// everything before the first kept item's row — including interleaved
	// unconvertible rows, which follow their preceding item — is compacted.
	// An unmoved split keeps the original count-based row boundary.
	if itemSplit < len(itemMsgIdx) && itemMsgIdx[itemSplit] < msgSplit {
		msgSplit = itemMsgIdx[itemSplit]
	}
	toCompact := active[:msgSplit]

	if ca.notify.OnStart != nil {
		ca.notify.OnStart()
	}
	var span *tracing.SpanHandle
	if args.StartSpan != nil {
		span = args.StartSpan()
	}
	span.Set("before_items", len(active))
	span.Set("after_items", 1+(len(active)-len(toCompact)))

	resp, err := ca.summaryModel.GetResponse(ctx, agents.ModelRequest{
		SystemInstructions: ca.summaryPrompt,
		Input:              toSummarize,
	})
	if err != nil {
		return fmt.Errorf("compaction adapter: summarizing: %w", err)
	}

	summaryText := agents.ExtractOutputText(resp.Output)
	if summaryText == "" {
		return nil
	}

	// A checkpoint, not a system message appended at the end. The distinction is
	// what removed the front-loading hack this replaced: a checkpoint carries
	// the retained tail inside it and truncates the path walk, so the model sees
	// [summary, kept…] by construction rather than because the reader was taught
	// to hoist one row to the front.
	excluded := make([]string, 0, len(toCompact))
	compactIDs := make([]int64, len(toCompact))
	for i, row := range toCompact {
		compactIDs[i] = row.ID
		excluded = append(excluded, row.EntryID)
	}
	retained, err := ca.retainedItems(active[msgSplit:])
	if err != nil {
		return fmt.Errorf("compaction adapter: encoding retained tail: %w", err)
	}
	summary, err := agents.NewCompactionEntry(agents.CompactionPayload{
		Summary:     agents.SummaryMarker + "\n\n" + summaryText,
		ExcludedIDs: excluded,
	}, retained)
	if err != nil {
		return fmt.Errorf("compaction adapter: encoding summary: %w", err)
	}
	summary.Display = &agents.ItemDisplay{Kind: agents.DisplayMessage, Text: summaryText}

	beforeCount := len(active)

	applied, err := ca.persistCompaction(ctx, compactIDs, summary)
	if err != nil {
		return fmt.Errorf("compaction adapter: persisting: %w", err)
	}
	if !applied {
		// The rows we planned to compact vanished before the write — the session
		// was deleted concurrently. Nothing changed, so don't fire OnDone with
		// counts for a compaction that never happened.
		return nil
	}

	afterCount := 1 + (len(active) - len(toCompact))
	if ca.notify.OnDone != nil {
		ca.notify.OnDone(beforeCount, afterCount)
	}
	return nil
}

// retainedItems projects the entries kept after the split into the items the
// checkpoint carries verbatim.
func (ca *CompactionAdapter) retainedItems(kept []entryRow) ([]agents.TResponseInputItem, error) {
	entries := make([]agents.SessionEntry, 0, len(kept))
	for i := range kept {
		var e agents.SessionEntry
		if json.Unmarshal([]byte(kept[i].Entry), &e) != nil {
			continue
		}
		entries = append(entries, e)
	}
	return agents.ProjectEntries(entries, nil)
}

// persistCompaction marks the folded entries compacted and appends the
// checkpoint in one transaction, reporting whether it applied. It guards
// against a concurrent session delete: if the UPDATE touches no rows the target
// entries are gone (the session was deleted between loading the history and
// this write), so it skips the checkpoint rather than orphan one in a session
// that no longer exists.
func (ca *CompactionAdapter) persistCompaction(ctx context.Context, compactIDs []int64, summary agents.SessionEntry) (bool, error) {
	applied := false
	err := ca.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		res, err := tx.NewUpdate().Model((*entryRow)(nil)).
			Set("compacted = ?", true).
			Where("id IN (?)", bun.List(compactIDs)).
			Exec(ctx)
		if err != nil {
			return err
		}
		// Only skip when the driver positively reports zero rows; if it cannot
		// report (err != nil), fall through and append as before.
		if n, err := res.RowsAffected(); err == nil && n == 0 {
			return nil
		}
		// The checkpoint's parent is the branch tip AFTER the fold, which is why
		// this runs inside the same transaction: appending against the pre-fold
		// tip would parent it at an entry it just folded away.
		if err := ca.appendTo(ctx, tx, summary); err != nil {
			return err
		}
		applied = true
		return nil
	})
	return applied, err
}
