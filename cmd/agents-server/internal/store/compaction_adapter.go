package store

import (
	"context"
	"fmt"
	"time"

	"github.com/openai/openai-go/v3/responses"
	"github.com/uptrace/bun"

	"github.com/zzir/agents-go/agents"
)

// CompactionCallback is called after a successful compaction with the item
// counts before and after the operation.
type CompactionCallback func(before, after int)

// CompactionAdapter wraps a SessionAdapter with provider-agnostic compaction
// that soft-deletes old messages (marks them compacted=true) rather than
// removing them, so the agents-server UI can still display them.
type CompactionAdapter struct {
	*SessionAdapter
	summaryModel  agents.Model
	threshold     int
	windowSize    int
	summaryPrompt string
	onCompaction  CompactionCallback
}

var (
	_ agents.Session                = (*CompactionAdapter)(nil)
	_ agents.CompactionAwareSession = (*CompactionAdapter)(nil)
)

// NewCompactionAdapter wraps sa with soft-delete compaction.
func NewCompactionAdapter(
	sa *SessionAdapter,
	summaryModel agents.Model,
	threshold, windowSize int,
	summaryPrompt string,
	onCompaction CompactionCallback,
) *CompactionAdapter {
	if threshold <= 0 {
		threshold = 20
	}
	if windowSize <= 0 {
		windowSize = 10
	}
	if summaryPrompt == "" {
		summaryPrompt = agents.DefaultSummaryPrompt
	}
	return &CompactionAdapter{
		SessionAdapter: sa,
		summaryModel:   summaryModel,
		threshold:      threshold,
		windowSize:     windowSize,
		summaryPrompt:  summaryPrompt,
		onCompaction:   onCompaction,
	}
}

// RunCompaction implements agents.CompactionAwareSession. It marks older
// messages as compacted and inserts a summary message, keeping the most
// recent windowSize non-compacted messages intact.
func (ca *CompactionAdapter) RunCompaction(ctx context.Context, args agents.CompactionArgs) error {
	var active []Message
	if err := ca.db.NewSelect().Model(&active).
		Where("session_id = ?", ca.sessionID).
		Where("compacted = ?", false).
		OrderExpr("id ASC").
		Scan(ctx); err != nil {
		return fmt.Errorf("compaction adapter: loading active messages: %w", err)
	}

	if !args.Force && len(active)-ca.windowSize < ca.threshold {
		return nil
	}

	if ca.windowSize >= len(active) {
		return nil
	}

	toCompact := active[:len(active)-ca.windowSize]

	items := make([]agents.TResponseInputItem, 0, len(toCompact))
	for _, m := range toCompact {
		if m.Item == "" || m.Item == "{}" || m.Item == "null" {
			continue
		}
		item, err := agents.UnmarshalInputItem([]byte(m.Item))
		if err != nil {
			continue
		}
		items = append(items, item)
	}

	if len(items) == 0 {
		return nil
	}

	if agents.IsSingleSummary(items) {
		return nil
	}

	resp, err := ca.summaryModel.GetResponse(ctx, agents.ModelRequest{
		SystemInstructions: ca.summaryPrompt,
		Input:              items,
	})
	if err != nil {
		return fmt.Errorf("compaction adapter: summarizing: %w", err)
	}

	summaryText := agents.ExtractOutputText(resp.Output)
	if summaryText == "" {
		return nil
	}

	summaryItem := responses.ResponseInputItemParamOfMessage(
		agents.SummaryMarker+"\n\n"+summaryText,
		responses.EasyInputMessageRoleSystem,
	)
	raw, err := agents.MarshalInputItem(summaryItem)
	if err != nil {
		return fmt.Errorf("compaction adapter: marshaling summary: %w", err)
	}

	beforeCount := len(active)

	err = ca.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		ids := make([]int64, len(toCompact))
		for i, m := range toCompact {
			ids[i] = m.ID
		}
		if _, err := tx.NewUpdate().Model((*Message)(nil)).
			Set("compacted = ?", true).
			Where("id IN (?)", bun.List(ids)).
			Exec(ctx); err != nil {
			return err
		}

		now := time.Now().UTC()
		summaryMsg := &Message{
			SessionID: ca.sessionID,
			RunID:     ca.runID,
			Role:      "compaction",
			Content:   summaryText,
			Item:      string(raw),
			CreatedAt: now,
		}
		if _, err := tx.NewInsert().Model(summaryMsg).Exec(ctx); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("compaction adapter: persisting: %w", err)
	}

	afterCount := 1 + (len(active) - len(toCompact))
	if ca.onCompaction != nil {
		ca.onCompaction(beforeCount, afterCount)
	}
	return nil
}
