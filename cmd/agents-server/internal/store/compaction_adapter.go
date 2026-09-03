package store

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/uptrace/bun"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/compaction"
	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/tracing"
)

// DefaultCompactionThresholdTokens is the estimated history size a pass fires
// at when the agent config names none; the Context panel reports the same number.
const DefaultCompactionThresholdTokens = 50000

// defaultCompactionWindow is the entry count the kept tail defaults to.
const defaultCompactionWindow = 10

// CompactionNotifier receives compaction lifecycle notifications: OnStart
// right before the summarization request, OnDone after a successful pass
// with the item counts before and after.
type CompactionNotifier struct {
	OnStart func()
	OnDone  func(before, after int)
}

// CompactionAdapter wraps an EntryStore with compaction that soft-deletes
// folded entries (compacted=true) so the UI can still show what was folded away.
type CompactionAdapter struct {
	*EntryStore
	summaryModel agents.Model
	// threshold is in TOKENS: a pass fires when the active history sizes
	// past it.
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
		threshold = DefaultCompactionThresholdTokens
	}
	if windowSize <= 0 {
		windowSize = defaultCompactionWindow
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
// windowSize non-compacted entries intact. Only the ACTIVE branch is sized
// and folded — invariant 24.
func (ca *CompactionAdapter) RunCompaction(ctx context.Context, args session.CompactionArgs) error {
	// The generation's rows WITHOUT bodies (the lifted columns answer the
	// check), through scoped like every other read.
	var rows []entryRow
	if err := ca.scoped(ca.db.NewSelect().Model(&rows)).
		ExcludeColumn("entry").
		OrderExpr("seq ASC").
		Scan(ctx); err != nil {
		return fmt.Errorf("compaction adapter: loading entries: %w", err)
	}
	onPath, err := ca.activeBranchOfRows(ctx, ca.ref, rows)
	if err != nil {
		return fmt.Errorf("compaction adapter: resolving the active branch: %w", err)
	}
	var active []entryRow
	for i := range rows {
		if onPath[rows[i].EntryID] && !rows[i].Compacted {
			active = append(active, rows[i])
		}
	}

	if !args.Force && activeTokens(active) < ca.threshold {
		return nil
	}

	if ca.windowSize >= len(active) {
		return nil
	}

	// Only a firing pass needs bodies, and only the active rows'.
	bodies, err := ca.entryBodies(ctx, ca.ref, rowIDs(active))
	if err != nil {
		return fmt.Errorf("compaction adapter: loading active entries: %w", err)
	}

	// Convert every active entry to its replayable item, remembering its row.
	// Rows that don't convert follow their preceding convertible neighbor.
	entries := make([]session.Entry, 0, len(active))
	itemMsgIdx := make([]int, 0, len(active))
	for i := range active {
		e, ok := bodies[active[i].ID]
		if !ok {
			continue
		}
		if e.Kind != session.EntryKindItem || len(e.Item) == 0 {
			continue
		}
		// Adapted like foreign replay (drop reasoning items, strip
		// provider-assigned ids): the summary reads content, not provenance.
		raw := adaptForeignItemJSON(e.Item)
		if raw == nil {
			continue
		}
		normalized := NormalizeItemJSON(raw)
		if _, err := session.UnmarshalInputItem(normalized); err != nil {
			continue
		}
		entries = append(entries, session.Entry{Kind: session.EntryKindItem, Item: normalized})
		itemMsgIdx = append(itemMsgIdx, i)
	}

	// The count-based split in row space, translated to item space.
	msgSplit := len(active) - ca.windowSize
	itemSplit := 0
	for itemSplit < len(entries) && itemMsgIdx[itemSplit] < msgSplit {
		itemSplit++
	}
	if itemSplit == 0 {
		return nil // nothing summarizable below the window
	}

	// Snap the split to a group boundary so it cannot cut a function_call /
	// output pair; 0 means no valid prefix exists, so skip this pass.
	itemSplit = compaction.SafeSplit(entries, itemSplit)
	if itemSplit <= 0 {
		return nil
	}

	// Summarizing a summary produces a paraphrase of a paraphrase.
	if compaction.IsSummaryOnly(entries[:itemSplit]) {
		return nil
	}

	// The folded prefix goes to the summary model as ONE plain-text
	// transcript under a single user message — invariant 26.
	transcript := renderTranscript(entries[:itemSplit])
	if transcript == "" {
		return nil
	}

	// Map the safe item split back to row space: when it moved, everything
	// before the first kept item's row is compacted.
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
		Input:              agents.InputItemsFromText(transcript),
	})
	if err != nil {
		return fmt.Errorf("compaction adapter: summarizing: %w", err)
	}

	summaryText := session.ExtractOutputText(resp.Output)
	if summaryText == "" {
		return nil
	}

	// A checkpoint names what it folded (ExcludedIDs) and carries the
	// summary; the kept tail stays in the session — invariant 24.
	excluded := make([]string, 0, len(toCompact))
	compactIDs := make([]string, len(toCompact))
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
		// The rows vanished before the write (a concurrent session delete);
		// nothing changed, so no OnDone.
		return nil
	}

	afterCount := 1 + (len(active) - len(toCompact))
	if ca.notify.OnDone != nil {
		ca.notify.OnDone(beforeCount, afterCount)
	}
	return nil
}

// activeTokens sizes the non-compacted history the way the threshold
// compares it (ActiveContextTokens), from the lifted columns only.
func activeTokens(active []entryRow) int {
	sizes := make([]ContextSize, 0, len(active))
	for i := range active {
		sizes = append(sizes, sizeOfRow(active[i]))
	}
	return ActiveContextTokens(sizes)
}

// sizeOfRow reduces a row to what sizing needs, from its columns alone.
func sizeOfRow(row entryRow) ContextSize {
	out := ContextSize{
		Est:        row.EstTokens,
		Checkpoint: row.Kind == string(session.EntryKindCompaction),
	}
	if row.Usage != "" {
		var u session.RequestUsage
		if json.Unmarshal([]byte(row.Usage), &u) == nil {
			out.Usage = &u
		}
	}
	return out
}

// ContextSize is one entry reduced to what sizing needs: what a call priced,
// what it estimates to, and whether it is a compaction checkpoint — all row columns.
type ContextSize struct {
	Usage      *session.RequestUsage
	Est        int
	Checkpoint bool
}

// ActiveContextTokens sizes a non-compacted history in tokens: the most
// recent usage-bearing entry prices everything up to itself, the tail after
// it is estimated, and a fold newer than that pricing discards it — invariant 28.
func ActiveContextTokens(sizes []ContextSize) int {
	lastUsage, lastFold := -1, -1
	for i := range sizes {
		if sizes[i].Usage != nil && sizes[i].Usage.TotalTokens > 0 {
			lastUsage = i
		}
		if sizes[i].Checkpoint {
			lastFold = i
		}
	}
	total := 0
	if lastFold > lastUsage {
		for i := range sizes {
			total += sizes[i].Est
		}
		return total
	}
	if lastUsage >= 0 {
		total = int(sizes[lastUsage].Usage.TotalTokens)
	}
	for i := lastUsage + 1; i < len(sizes); i++ {
		total += sizes[i].Est
	}
	return total
}

// estimateFold sizes the context on either side of the pass for the
// checkpoint to report. Estimates by construction (CharEstimator), not a bill.
func estimateFold(active, folded []entryRow, summaryText string) (before, after int) {
	for i := range active {
		before += active[i].EstTokens
	}
	after = before
	for i := range folded {
		after -= folded[i].EstTokens
	}
	// The summary replaces what it folded, so it counts toward the new size.
	after += len(summaryText) / 4
	return before, after
}

// rowIDs is the rows' primary keys, for a bodies fetch.
func rowIDs(rows []entryRow) []string {
	ids := make([]string, len(rows))
	for i := range rows {
		ids[i] = rows[i].ID
	}
	return ids
}

// renderTranscript flattens the folded items into the plain-text record the
// summarization request carries; lossy on purpose (invariant 26).
func renderTranscript(entries []session.Entry) string {
	var b strings.Builder
	for _, e := range entries {
		line := renderItemText(e.Item)
		if line == "" {
			continue
		}
		b.WriteString(line)
		b.WriteString("\n\n")
	}
	return strings.TrimSpace(b.String())
}

// renderItemText renders one normalized item as transcript text; "" for items
// with nothing to say (unparseable, or types with no content).
func renderItemText(raw json.RawMessage) string {
	var it struct {
		Type      string          `json:"type"`
		Role      string          `json:"role"`
		Content   json.RawMessage `json:"content"`
		Name      string          `json:"name"`
		Arguments string          `json:"arguments"`
		Output    json.RawMessage `json:"output"`
	}
	if json.Unmarshal(raw, &it) != nil {
		return ""
	}
	switch {
	case it.Type == "function_call":
		return "[assistant called tool " + it.Name + " with arguments " + it.Arguments + "]"
	case it.Type == "function_call_output":
		out := jsonAsText(it.Output)
		if out == "" {
			return ""
		}
		return "[tool output]\n" + out
	case it.Role != "":
		text := contentAsText(it.Content)
		if text == "" {
			return ""
		}
		return strings.ToUpper(it.Role[:1]) + it.Role[1:] + ":\n" + text
	}
	return ""
}

// contentAsText joins a message's content: either a bare string or an array of
// parts whose text fields carry the words.
func contentAsText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var texts []string
	for _, p := range parts {
		if p.Text != "" {
			texts = append(texts, p.Text)
		}
	}
	return strings.Join(texts, "\n")
}

// jsonAsText unwraps a JSON string, and falls back to the raw JSON for
// structured payloads — the summary model can read either.
func jsonAsText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

// persistCompaction marks the folded entries compacted and appends the checkpoint in
// one transaction; an UPDATE touching no rows (session deleted) writes no checkpoint.
func (ca *CompactionAdapter) persistCompaction(ctx context.Context, compactIDs []string, summary session.Entry) (bool, error) {
	applied := false
	err := ca.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if err := ca.lockSessionIn(ctx, tx); err != nil {
			return err
		}
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
		// The checkpoint's parent is the branch tip AFTER the fold, so refold
		// the append point before the append reads it (a strict-prefix fold makes this a no-op).
		if err := ca.refreshAppendPointIn(ctx, tx); err != nil {
			return err
		}
		if err := ca.appendTo(ctx, tx, summary); err != nil {
			return err
		}
		applied = true
		return nil
	})
	return applied, err
}
