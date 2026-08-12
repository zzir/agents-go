package store

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/uptrace/bun"

	"github.com/zzir/agents-go/agents/session"
)

// contextItemLimit is how many of the heaviest entries the report carries.
const contextItemLimit = 5

// contextLabelChars caps an item's label; the panel shows one line.
const contextLabelChars = 80

// ContextReport is what a session's active branch occupies of the model's
// context window.
//
// Its figures are not one ruler and are never mixed (README invariant 28):
// InputTokens and the cache split are the provider's counts for the last model
// call; CompactionTokens is what the compaction pass compares (mostly that same
// provider number, plus an estimate for the turns since — ActiveContextTokens);
// the Items sizes and Prompt are character estimates, good for ranking and not
// for arithmetic against either of the others.
type ContextReport struct {
	Model string `json:"model,omitempty"`
	// ContextWindow is the agent config's declared window in tokens; 0 means
	// unconfigured and the client shows occupancy without a denominator.
	ContextWindow int `json:"context_window,omitempty"`

	// InputTokens is what the LAST model call on the branch sent — the history,
	// prompt and tool schemas that are in the window right now. OutputTokens is
	// that same call's completion.
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	// CachedTokens / CacheWriteTokens split that call's input by cache
	// disposition, for providers that report it.
	CachedTokens     int64 `json:"cached_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`

	// SessionInputTokens / SessionOutputTokens total every model call on the
	// branch. Input counts re-sent history once per call, so it runs far ahead
	// of InputTokens by design — it is a spend figure, not a window figure.
	SessionInputTokens  int64 `json:"session_input_tokens"`
	SessionOutputTokens int64 `json:"session_output_tokens"`

	// Growth is each model call's input tokens in order — the curve the panel
	// draws, where a compaction pass shows up as the drop it caused.
	Growth []int64 `json:"growth,omitempty"`

	// CompactionEnabled reports whether the pass runs at all; Threshold is what
	// it fires at (the default filled in when the config names none) and Tokens
	// is what it compares — ActiveContextTokens over the same history, so the
	// number the panel draws is the number that trips.
	CompactionEnabled   bool `json:"compaction_enabled"`
	CompactionThreshold int  `json:"compaction_threshold,omitempty"`
	CompactionTokens    int  `json:"compaction_tokens"`

	// Items are the heaviest entries still in context, largest first.
	Items []ContextItem `json:"items,omitempty"`

	// Prompt is what the session's last build put in front of the conversation
	// — the instruction layers and the tool surface, in the same estimated
	// characters as CompactionTokens. Absent until a run has built once.
	Prompt *PromptProfile `json:"prompt,omitempty"`
}

// ContextItem is one entry's estimated share of the context. The ranking is
// worth acting on; the digits are an estimate scaled from character counts.
type ContextItem struct {
	// Kind is the entry's display kind (message / tool_output / reasoning / …).
	Kind   string `json:"kind"`
	Label  string `json:"label"`
	Tokens int    `json:"tokens"`
	// Anchor is what the client scrolls to: a tool call id when the entry is
	// part of a tool exchange, the entry id otherwise. RunID is the fallback
	// for an entry the timeline renders under a turn rather than on its own.
	Anchor string `json:"anchor,omitempty"`
	RunID  string `json:"run_id,omitempty"`
	// ResponseID names the model call that produced the entry, which is how a
	// non-tool item finds its generation span (a tool item uses Anchor, which
	// is the call id its function span records).
	ResponseID string `json:"response_id,omitempty"`
}

// ContextReport measures what the session named by ref currently puts in its
// model's context window. Only the ACTIVE branch counts: an abandoned attempt
// is still recorded but is no longer sent. Compacted entries keep their usage
// (the call happened) but leave the item list and the estimate, since the model
// no longer sees them.
//
// It reads the session in two passes, because the whole point is not to read
// the content of entries nobody asked about: the first pass takes only the
// lifted columns (usage, estimate, links — no entry bodies), and the second
// fetches the bodies of the few entries that reach the item list. What it costs
// is therefore the session's ROW COUNT plus five entries, not its size.
//
// The report describes the session alone; the caller fills in Model,
// ContextWindow and the compaction settings, which live on the agent config.
func (s *EntryStore) ContextReport(ctx context.Context, ref session.Ref) (*ContextReport, error) {
	var rows []entryRow
	if err := s.db.NewSelect().Model(&rows).
		ExcludeColumn("entry").
		Where("session_id = ?", ref.ID).Where("gen = ?", ref.Gen).
		OrderExpr("id ASC").Scan(ctx); err != nil {
		return nil, fmt.Errorf("reading entries for the context report of session %s: %w", ref.ID, err)
	}
	onPath, err := s.activeBranchOfRows(ctx, ref, rows)
	if err != nil {
		return nil, err
	}

	rep := &ContextReport{}
	var ranked []entryRow
	var active []ContextSize // the history the compaction pass would size
	for i := range rows {
		if !onPath[rows[i].EntryID] {
			continue
		}
		size := sizeOfRow(rows[i])
		if u := size.Usage; u != nil {
			rep.SessionInputTokens += u.InputTokens
			rep.SessionOutputTokens += u.OutputTokens
			rep.Growth = append(rep.Growth, u.InputTokens)
			rep.InputTokens = u.InputTokens
			rep.OutputTokens = u.OutputTokens
			rep.CachedTokens = u.InputTokensDetails.CachedTokens
			rep.CacheWriteTokens = u.InputTokensDetails.CacheWriteTokens
		}
		if rows[i].Compacted {
			continue
		}
		active = append(active, size)
		if size.Est > 0 {
			ranked = append(ranked, rows[i])
		}
	}

	rep.CompactionTokens = ActiveContextTokens(active)
	slices.SortStableFunc(ranked, func(a, b entryRow) int { return b.EstTokens - a.EstTokens })
	items, err := s.contextItems(ctx, ref, ranked)
	if err != nil {
		return nil, err
	}
	rep.Items = items
	return rep, nil
}

// activeBranchOfRows marks the active branch over rows read without bodies.
// Links and kinds are columns; the one thing that is not is a leaf marker's
// target, which lives in its payload — so those few bodies are fetched, and the
// walk itself is activeBranch over just enough of an Entry, not a second
// definition of "active". Shared by the context report and the compaction
// pass, so the number the panel draws is measured over the rows the pass folds.
func (s *EntryStore) activeBranchOfRows(ctx context.Context, ref session.Ref, rows []entryRow) (map[string]bool, error) {
	var leaves []int64
	for i := range rows {
		if rows[i].Kind == string(session.EntryKindLeaf) {
			leaves = append(leaves, rows[i].ID)
		}
	}
	bodies := map[int64]session.Entry{}
	if len(leaves) > 0 {
		var err error
		if bodies, err = s.entryBodies(ctx, ref, leaves); err != nil {
			return nil, err
		}
	}
	skeletons := make([]session.Entry, 0, len(rows))
	for i := range rows {
		if e, ok := bodies[rows[i].ID]; ok {
			skeletons = append(skeletons, e)
			continue
		}
		skeletons = append(skeletons, session.Entry{
			ID:       rows[i].EntryID,
			ParentID: rows[i].ParentID,
			Kind:     session.EntryKind(rows[i].Kind),
		})
	}
	return activeBranch(skeletons), nil
}

// entryBodies decodes the named rows' entries, keyed by row id.
func (s *EntryStore) entryBodies(ctx context.Context, ref session.Ref, ids []int64) (map[int64]session.Entry, error) {
	var rows []entryRow
	if err := s.db.NewSelect().Model(&rows).
		Column("id", "entry").
		Where("session_id = ?", ref.ID).Where("gen = ?", ref.Gen).
		Where("id IN (?)", bun.List(ids)).Scan(ctx); err != nil {
		return nil, fmt.Errorf("reading entry bodies for session %s: %w", ref.ID, err)
	}
	out := make(map[int64]session.Entry, len(rows))
	for i := range rows {
		var e session.Entry
		if json.Unmarshal([]byte(rows[i].Entry), &e) != nil {
			continue
		}
		out[rows[i].ID] = e
	}
	return out, nil
}

// contextItems turns the heaviest rows into panel items, reading the bodies of
// those few (and of every update entry, which amends a display and is tiny).
func (s *EntryStore) contextItems(ctx context.Context, ref session.Ref, ranked []entryRow) ([]ContextItem, error) {
	top := ranked[:min(len(ranked), contextItemLimit)]
	if len(top) == 0 {
		return nil, nil
	}
	ids := make([]int64, 0, len(top))
	for i := range top {
		ids = append(ids, top[i].ID)
	}
	var updates []entryRow
	if err := s.db.NewSelect().Model(&updates).
		Column("id").
		Where("session_id = ?", ref.ID).Where("gen = ?", ref.Gen).
		Where("kind = ?", string(session.EntryKindUpdate)).Scan(ctx); err != nil {
		return nil, fmt.Errorf("reading display updates for session %s: %w", ref.ID, err)
	}
	for i := range updates {
		ids = append(ids, updates[i].ID)
	}

	bodies, err := s.entryBodies(ctx, ref, ids)
	if err != nil {
		return nil, err
	}
	entries := make([]session.Entry, 0, len(bodies))
	for _, id := range ids {
		if e, ok := bodies[id]; ok {
			entries = append(entries, e)
		}
	}
	folded := make(map[string]session.Entry, len(entries))
	for _, e := range session.FoldUpdates(entries) {
		folded[e.ID] = e
	}

	items := make([]ContextItem, 0, len(top))
	for _, row := range top {
		e, ok := folded[row.EntryID]
		if !ok {
			continue
		}
		items = append(items, ContextItem{
			Kind:       contextKindOf(e),
			Label:      contextLabelOf(e),
			Tokens:     row.EstTokens,
			Anchor:     contextAnchorOf(e),
			RunID:      row.RunID,
			ResponseID: e.ResponseID,
		})
	}
	return items, nil
}

// contextKindOf names what an entry is, preferring the display projection the
// runner recorded over the storage kind a reader would have to interpret.
func contextKindOf(e session.Entry) string {
	if e.Display != nil && e.Display.Kind != "" {
		return e.Display.Kind
	}
	if e.Kind == session.EntryKindCompaction {
		return "compaction"
	}
	return roleOf(e)
}

// contextLabelOf is the one line naming the item in the panel.
func contextLabelOf(e session.Entry) string {
	if d := e.Display; d != nil {
		switch {
		case d.ToolName != "" && d.Title != "" && d.Title != d.ToolName:
			return trimLine(d.ToolName + " · " + d.Title)
		case d.ToolName != "":
			return trimLine(d.ToolName)
		case d.Title != "":
			return trimLine(d.Title)
		}
	}
	return trimLine(contentOf(e))
}

// contextAnchorOf is what the client scrolls to. A tool exchange is rendered as
// one card keyed by call id; everything else is anchored by its entry.
func contextAnchorOf(e session.Entry) string {
	if e.Display != nil && e.Display.CallID != "" {
		return e.Display.CallID
	}
	return e.ID
}

// trimLine squashes a body of text into one line of at most contextLabelChars.
// It cuts on RUNES: labels are routinely CJK, and a byte cut would end them
// mid-character.
func trimLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= contextLabelChars {
		return s
	}
	return strings.TrimSpace(string(r[:contextLabelChars])) + "…"
}
