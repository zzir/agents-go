package store

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"

	"github.com/uptrace/bun"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/compaction"
	"github.com/zzir/agents-go/agents/session"
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
	summaryModel agents.Model
	// threshold is in TOKENS: a pass fires when the active history sizes past
	// it. Entry counts said nothing about context pressure — twenty short
	// turns are a few thousand tokens, twenty tool dumps can be a hundred
	// times that.
	threshold int
	// windowSize stays in ENTRIES: the kept tail needs pairing-safe cutting,
	// which is an entry-boundary concern, not a token one.
	windowSize    int
	summaryPrompt string
	notify        CompactionNotifier
}

var (
	_ session.Storage         = (*CompactionAdapter)(nil)
	_ session.CompactionAware = (*CompactionAdapter)(nil)
)

// NewCompactionAdapter wraps store with soft-delete compaction. threshold is
// in tokens (0 = default 50k), windowSize in entries (0 = default 10).
func NewCompactionAdapter(
	store *EntryStore,
	summaryModel agents.Model,
	threshold, windowSize int,
	summaryPrompt string,
	notify CompactionNotifier,
) *CompactionAdapter {
	if threshold <= 0 {
		threshold = 50000
	}
	if windowSize <= 0 {
		windowSize = 10
	}
	summaryPrompt = cmp.Or(summaryPrompt, session.DefaultSummaryPrompt)
	return &CompactionAdapter{
		EntryStore:    store,
		summaryModel:  summaryModel,
		threshold:     threshold,
		windowSize:    windowSize,
		summaryPrompt: summaryPrompt,
		notify:        notify,
	}
}

// RunCompaction implements session.CompactionAware. It marks older entries
// compacted and appends a compaction checkpoint, keeping the most recent
// windowSize non-compacted entries intact.
func (ca *CompactionAdapter) RunCompaction(ctx context.Context, args session.CompactionArgs) error {
	var active []entryRow
	// Through scoped, like every other read: the generation is part of the
	// address, and a select that names only the session id folds another
	// generation's history into this one's compaction pass.
	if err := ca.scoped(ca.db.NewSelect().Model(&active)).
		Where("compacted = ?", false).
		OrderExpr("id ASC").
		Scan(ctx); err != nil {
		return fmt.Errorf("compaction adapter: loading active entries: %w", err)
	}

	if !args.Force && activeTokens(active) < ca.threshold {
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
	items := make([]agents.InputItem, 0, len(active))
	entries := make([]session.Entry, 0, len(active))
	itemMsgIdx := make([]int, 0, len(active))
	for i := range active {
		var e session.Entry
		if json.Unmarshal([]byte(active[i].Entry), &e) != nil {
			continue
		}
		if e.Kind != session.EntryKindItem || len(e.Item) == 0 {
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
		item, err := session.UnmarshalInputItem(normalized)
		if err != nil {
			continue
		}
		items = append(items, item)
		// The grouping below reads the wire JSON, so carry it alongside rather
		// than re-marshaling what we just decoded.
		entries = append(entries, session.Entry{Kind: session.EntryKindItem, Item: normalized})
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

	resp, err := ca.summaryModel.Respond(ctx, agents.ModelRequest{
		SystemInstructions: ca.summaryPrompt,
		Input:              toSummarize,
	})
	if err != nil {
		return fmt.Errorf("compaction adapter: summarizing: %w", err)
	}

	summaryText := session.ExtractOutputText(resp.Output)
	if summaryText == "" {
		return nil
	}

	// A checkpoint, not a system message appended at the end: it names what it
	// folded (ExcludedIDs) and carries the summary that stands in for it. The
	// kept tail is NOT inside it — those entries stay in the session and the
	// projection reads them from there, so a tail entry later popped is simply
	// gone rather than living on in a copy (session.CompactionPayload).
	excluded := make([]string, 0, len(toCompact))
	compactIDs := make([]int64, len(toCompact))
	for i, row := range toCompact {
		compactIDs[i] = row.ID
		excluded = append(excluded, row.EntryID)
	}
	before, after := estimateFold(active, toCompact, summaryText)
	summary, err := session.NewCompactionEntry(session.CompactionPayload{
		Summary:      session.SummaryMarker + "\n\n" + summaryText,
		ExcludedIDs:  excluded,
		TokensBefore: before,
		TokensAfter:  after,
	})
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

// activeTokens sizes the non-compacted history in tokens. The most recent
// entry carrying real usage prices everything up to and including itself —
// exactly one entry per response carries usage, and its call's input covered
// the history before it (session_entry.go: "a reader estimating how large it
// has grown reads the most recent one"). Entries after it, which no call has
// priced yet, are byte-estimated. With no usage anywhere (a fresh or
// never-successful session) everything is estimated.
func activeTokens(active []entryRow) int {
	est := compaction.CharEstimator{}
	entries := make([]session.Entry, len(active))
	last := -1
	for i := range active {
		if json.Unmarshal([]byte(active[i].Entry), &entries[i]) != nil {
			continue
		}
		if entries[i].Usage != nil && entries[i].Usage.TotalTokens > 0 {
			last = i
		}
	}
	total := 0
	if last >= 0 {
		total = int(entries[last].Usage.TotalTokens)
	}
	for i := last + 1; i < len(entries); i++ {
		total += est.Estimate(entries[i])
	}
	return total
}

// estimateFold sizes the context on either side of the pass, so the checkpoint
// can report what compaction bought without the reader recomputing it.
//
// Estimates by construction — CharEstimator is a character ratio, not a
// tokenizer. The point is to say "12k became 3k", not to predict a bill.
func estimateFold(active, folded []entryRow, summaryText string) (before, after int) {
	est := compaction.CharEstimator{}
	sizeOf := func(row entryRow) int {
		var e session.Entry
		if json.Unmarshal([]byte(row.Entry), &e) != nil {
			return 0
		}
		return est.Estimate(e)
	}
	for i := range active {
		before += sizeOf(active[i])
	}
	after = before
	for i := range folded {
		after -= sizeOf(folded[i])
	}
	// The summary replaces what it folded, so it counts toward the new size.
	after += len(summaryText) / 4
	return before, after
}

// persistCompaction marks the folded entries compacted and appends the
// checkpoint in one transaction, reporting whether it applied. It guards
// against a concurrent session delete: if the UPDATE touches no rows the target
// entries are gone (the session was deleted between loading the history and
// this write), so it skips the checkpoint rather than orphan one in a session
// that no longer exists.
func (ca *CompactionAdapter) persistCompaction(ctx context.Context, compactIDs []int64, summary session.Entry) (bool, error) {
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
