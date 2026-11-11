package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/uptrace/bun"

	"github.com/zzir/agents-go/agents/session"
)

// ContextReport is what a session's active branch occupies of the model's
// context window; its figures are on different rulers and never mixed —
// invariant 28. A sub-agent's own session is not in it, only its result text.
type ContextReport struct {
	Model string `json:"model,omitempty"`
	// ContextWindow is the agent's declared window in tokens; 0 shows occupancy
	// without a denominator.
	ContextWindow int `json:"context_window,omitempty"`

	// InputTokens is what the last model call on the branch sent, OutputTokens
	// that call's completion.
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	// CachedTokens and CacheWriteTokens split that call's input by cache
	// disposition, when the provider reports it.
	CachedTokens     int64 `json:"cached_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`

	// SessionInputTokens and SessionOutputTokens total every model call on the
	// branch: spend, not window.
	SessionInputTokens  int64 `json:"session_input_tokens"`
	SessionOutputTokens int64 `json:"session_output_tokens"`

	// Growth is each model call's input tokens in order; a compaction pass
	// shows as a drop.
	Growth []int64 `json:"growth,omitempty"`

	// CompactionEnabled reports whether the pass runs; Threshold is what it
	// fires at, Tokens what it compares.
	CompactionEnabled   bool `json:"compaction_enabled"`
	CompactionThreshold int  `json:"compaction_threshold,omitempty"`
	CompactionTokens    int  `json:"compaction_tokens"`
	// CompactionMode is the agent's: summary, reset or hybrid.
	CompactionMode string `json:"compaction_mode,omitempty"`

	// ConversationTokens is the estimated size of the transcript still in
	// context: active, uncompacted entries summed.
	ConversationTokens int `json:"conversation_tokens"`

	// Prompt is what the last build put in front of the conversation; absent
	// until a run has built once.
	Prompt *PromptProfile `json:"prompt,omitempty"`
}

// ContextReport measures what the session puts in its model's context
// window: the ACTIVE branch, from lifted columns — invariant 28. The caller
// fills in Model, ContextWindow and the compaction settings.
func (s *EntryStore) ContextReport(ctx context.Context) (*ContextReport, error) {
	var rows []entryRow
	if err := s.scoped(s.db.NewSelect().Model(&rows)).
		ExcludeColumn("entry").
		OrderExpr("seq ASC").Scan(ctx); err != nil {
		return nil, fmt.Errorf("reading entries for the context report of session %s: %w", s.ref.ID, err)
	}
	on, err := s.activePath(ctx, rows)
	if err != nil {
		return nil, err
	}
	folded, err := s.foldedOn(ctx, rows, on)
	if err != nil {
		return nil, err
	}

	rep := &ContextReport{}
	var active []ContextSize // the history the compaction pass would size
	for i := range rows {
		if !on[rows[i].EntryID] {
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
		if folded[rows[i].EntryID] {
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
func (s *EntryStore) UsageTotals(ctx context.Context) (int, error) {
	var rows []entryRow
	if err := s.scoped(s.db.NewSelect().Model(&rows)).
		Column("usage").
		Where("usage IS NOT NULL").
		Scan(ctx); err != nil {
		return 0, fmt.Errorf("reading usage of session %s: %w", s.ref.ID, err)
	}
	total := 0
	for i := range rows {
		if u := sizeOfRow(rows[i]).Usage; u != nil {
			total += int(u.InputTokens + u.OutputTokens)
		}
	}
	return total, nil
}

// entryBodies decodes the named rows' entries, keyed by row id.
func (s *EntryStore) entryBodies(ctx context.Context, ids []string) (map[string]session.Entry, error) {
	var rows []entryRow
	if err := s.scoped(s.db.NewSelect().Model(&rows)).
		Column("id", "entry").
		Where("id IN (?)", bun.List(ids)).Scan(ctx); err != nil {
		return nil, fmt.Errorf("reading entry bodies for session %s: %w", s.ref.ID, err)
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
