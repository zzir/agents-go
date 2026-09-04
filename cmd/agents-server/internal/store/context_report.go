package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/uptrace/bun"

	"github.com/zzir/agents-go/agents/session"
)

// ContextReport is what a session's active branch occupies of the model's
// context window. Its figures are not one ruler and are never mixed —
// invariant 28. Sub-agents appear in none of them: a task runs on its own
// session, and only its result text lands here, as any tool output.
type ContextReport struct {
	Model string `json:"model,omitempty"`
	// ContextWindow is the agent config's declared window in tokens; 0 means
	// unconfigured and the client shows occupancy without a denominator.
	ContextWindow int `json:"context_window,omitempty"`

	// InputTokens is what the LAST model call on the branch sent — what is in
	// the window right now. OutputTokens is that same call's completion.
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	// CachedTokens / CacheWriteTokens split that call's input by cache
	// disposition, for providers that report it.
	CachedTokens     int64 `json:"cached_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`

	// SessionInputTokens / SessionOutputTokens total every model call on the
	// branch — a spend figure, not a window figure.
	SessionInputTokens  int64 `json:"session_input_tokens"`
	SessionOutputTokens int64 `json:"session_output_tokens"`

	// Growth is each model call's input tokens in order — the curve the panel
	// draws, where a compaction pass shows up as the drop it caused.
	Growth []int64 `json:"growth,omitempty"`

	// CompactionEnabled reports whether the pass runs; Threshold is what it
	// fires at and Tokens what it compares (ActiveContextTokens).
	CompactionEnabled   bool `json:"compaction_enabled"`
	CompactionThreshold int  `json:"compaction_threshold,omitempty"`
	CompactionTokens    int  `json:"compaction_tokens"`

	// ConversationTokens is the estimated size of the transcript still in
	// context — every active, uncompacted entry's estimate summed.
	ConversationTokens int `json:"conversation_tokens"`

	// Prompt is what the session's last build put in front of the
	// conversation (instruction layers, tool surface); absent until a run has built once.
	Prompt *PromptProfile `json:"prompt,omitempty"`
}

// ContextReport measures what the session named by ref currently puts in its
// model's context window: the ACTIVE branch only, from lifted columns (so
// its cost is the row count, not the size) — invariant 28. The caller fills
// in Model, ContextWindow and the compaction settings from the agent config.
func (s *EntryStore) ContextReport(ctx context.Context, ref session.Ref) (*ContextReport, error) {
	var rows []entryRow
	if err := s.db.NewSelect().Model(&rows).
		ExcludeColumn("entry").
		Where("session_id = ?", ref.ID).Where("gen = ?", ref.Gen).
		OrderExpr("seq ASC").Scan(ctx); err != nil {
		return nil, fmt.Errorf("reading entries for the context report of session %s: %w", ref.ID, err)
	}
	onPath, err := s.activeBranchOfRows(ctx, ref, rows)
	if err != nil {
		return nil, err
	}

	rep := &ContextReport{}
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
		rep.ConversationTokens += size.Est
	}

	rep.CompactionTokens = ActiveContextTokens(active)
	return rep, nil
}

// UsageTotals sums input plus output tokens of every model call on the
// session, every branch and compacted entry included — a workflow's token budget ruler.
func (s *EntryStore) UsageTotals(ctx context.Context, ref session.Ref) (int, error) {
	var rows []entryRow
	if err := s.db.NewSelect().Model(&rows).
		Column("usage").
		Where("session_id = ?", ref.ID).Where("gen = ?", ref.Gen).
		Where("usage IS NOT NULL").
		Scan(ctx); err != nil {
		return 0, fmt.Errorf("reading usage of session %s: %w", ref.ID, err)
	}
	total := 0
	for i := range rows {
		if u := sizeOfRow(rows[i]).Usage; u != nil {
			total += int(u.InputTokens + u.OutputTokens)
		}
	}
	return total, nil
}

// activeBranchOfRows marks the active branch over rows read without bodies;
// only the leaf markers' bodies are fetched. Shared by the report and the compaction pass.
func (s *EntryStore) activeBranchOfRows(ctx context.Context, ref session.Ref, rows []entryRow) (map[string]bool, error) {
	var leaves []string
	for i := range rows {
		if rows[i].Kind == string(session.EntryKindLeaf) {
			leaves = append(leaves, rows[i].ID)
		}
	}
	bodies := map[string]session.Entry{}
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
func (s *EntryStore) entryBodies(ctx context.Context, ref session.Ref, ids []string) (map[string]session.Entry, error) {
	var rows []entryRow
	if err := s.db.NewSelect().Model(&rows).
		Column("id", "entry").
		Where("session_id = ?", ref.ID).Where("gen = ?", ref.Gen).
		Where("id IN (?)", bun.List(ids)).Scan(ctx); err != nil {
		return nil, fmt.Errorf("reading entry bodies for session %s: %w", ref.ID, err)
	}
	out := make(map[string]session.Entry, len(rows))
	for i := range rows {
		var e session.Entry
		if json.Unmarshal([]byte(rows[i].Entry), &e) != nil {
			continue
		}
		out[rows[i].ID] = e
	}
	return out, nil
}
